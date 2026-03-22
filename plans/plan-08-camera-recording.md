# Plan 8: Camera Firmware — fMP4 Recording & Playback Support

**Status:** Not started
**Branch:** `rewrite`
**Depends on:** Plan 7 (camera core — capture pipeline, session, commands)
**Unlocks:** Plan 10 (viewer — HLS playback needs segments to exist)

---

## 1. Goal

Implement the camera-side recording pipeline: an fMP4 muxer that consumes H.264 and Opus frames from the capture pipeline, writes GOP-aligned 10-second segments to disk, manages the ring buffer with eviction, generates and updates the HLS manifest, and handles upload commands from the server (segment, init, manifest push).

After this plan, the camera continuously records to disk in fMP4 format, maintains a sliding-window HLS manifest, pushes manifest updates to the server, responds to `upload_segment` and `upload_init` commands, handles storage-full conditions, and recovers cleanly from crashes.

---

## 2. Crate Changes

### 2.1 Dependencies

**`camera/Cargo.toml`** — add:
```toml
[dependencies]
mp4 = "0.14"
toml = "0.8"
```

Start with the `mp4` crate for fMP4 writing. If it doesn't handle Opus-in-fMP4 well or adds too much friction, replace with a minimal custom fMP4 writer — the format subset we need (single video + single audio track, no B-frames, fixed segment duration) is small enough to implement directly using raw ISO BMFF box construction.

---

## 3. Implementation Details

### 3.1 fMP4 Muxer (`camera/src/recording/muxer.rs`)

The muxer runs as a Tokio task, consuming frames from the capture pipeline via a dedicated broadcast receiver (separate from the QUIC stream writers).

```rust
pub struct Muxer {
    /// Segment output directory
    segment_dir: PathBuf,
    /// Device ID prefix for segment naming
    device_id: String,
    /// Target segment duration
    segment_duration: Duration,
    /// Current segment state
    current_segment: Option<SegmentWriter>,
    /// Init segment bytes (regenerated on capture param change)
    init_segment: Option<Bytes>,
    /// Channel to notify session of completed segments
    segment_tx: mpsc::Sender<SegmentEvent>,
}

pub enum SegmentEvent {
    /// A segment has been finalized and written to disk.
    Finalized {
        segment_id: String,
        start_ts: u64,
        end_ts: u64,
        size_bytes: u64,
    },
    /// A segment has been evicted from the ring buffer.
    Evicted {
        segment_id: String,
    },
    /// The manifest has been updated (new manifest string).
    ManifestUpdated {
        manifest: String,
    },
    /// Init segment has been (re)generated.
    InitReady {
        data: Bytes,
    },
    /// Storage full — recording paused.
    StorageFull {
        free_bytes: u64,
    },
    /// Storage resumed — recording resumed.
    StorageResumed {
        free_bytes: u64,
    },
}
```

#### Muxer Loop

```rust
impl Muxer {
    /// Create a new muxer. Scans existing segments for crash recovery.
    pub async fn new(
        segment_dir: &Path,
        device_id: &str,
        segment_tx: mpsc::Sender<SegmentEvent>,
    ) -> Result<Self>;

    /// Run the muxer loop. Consumes frames until cancelled.
    ///
    /// On first video frame:
    ///   1. Generate init.mp4 with codec parameters
    ///   2. Emit InitReady event
    ///   3. Begin first segment
    ///
    /// On each video NAL:
    ///   - If IDR NAL and segment duration >= target: finalize current, start new
    ///   - Write NAL to current segment
    ///
    /// On each audio frame:
    ///   - Write to current segment
    ///
    /// On segment finalization:
    ///   1. Close segment file
    ///   2. Emit Finalized event (triggers recording_segment alert)
    ///   3. Update manifest
    ///   4. Emit ManifestUpdated event (triggers manifest push)
    ///   5. Check ring buffer, evict if needed
    pub async fn run(
        &mut self,
        video_rx: broadcast::Receiver<Bytes>,
        audio_rx: broadcast::Receiver<Bytes>,
        cancel: CancellationToken,
    ) -> Result<()>;
}
```

### 3.2 Segment Writer (`camera/src/recording/segment.rs`)

Writes a single fMP4 segment (`.m4s`) to disk.

```rust
struct SegmentWriter {
    /// File being written
    file: tokio::fs::File,
    /// Segment ID: {device_id}:{start_ts}
    segment_id: String,
    /// Segment start timestamp (Unix ms)
    start_ts: u64,
    /// Running duration
    duration: Duration,
    /// Bytes written
    size: u64,
}

impl SegmentWriter {
    /// Create a new segment file.
    fn create(dir: &Path, segment_id: &str) -> Result<Self>;

    /// Write a video sample to the segment.
    fn write_video(&mut self, nal: &[u8], timestamp: Duration) -> Result<()>;

    /// Write an audio sample to the segment.
    fn write_audio(&mut self, frame: &[u8], timestamp: Duration) -> Result<()>;

    /// Finalize the segment: flush, close, return metadata.
    fn finalize(self) -> Result<SegmentMetadata>;
}

struct SegmentMetadata {
    segment_id: String,
    start_ts: u64,
    end_ts: u64,
    size_bytes: u64,
    path: PathBuf,
}
```

### 3.3 Init Segment Generator (`camera/src/recording/init.rs`)

```rust
/// Generate an fMP4 init segment (moov box) from codec parameters.
///
/// Video: H.264 baseline, extracted SPS/PPS from first IDR
/// Audio: Opus, 48kHz mono
///
/// Returns raw init.mp4 bytes.
pub fn generate_init_segment(
    sps: &[u8],
    pps: &[u8],
) -> Result<Bytes>;
```

If the `mp4` crate makes this difficult (especially for Opus), implement directly by constructing the required ISO BMFF boxes:
- `ftyp` (isom, iso6, msdh, msix)
- `moov` → `mvhd` → `trak` (video) → `trak` (audio)
- Video `trak`: `tkhd`, `mdia` → `mdhd`, `hdlr`, `minf` → `stbl` with `avc1` sample entry
- Audio `trak`: `tkhd`, `mdia` → `mdhd`, `hdlr`, `minf` → `stbl` with `Opus` sample entry
- `mvex` → `trex` per track

### 3.4 Ring Buffer (`camera/src/recording/ring_buffer.rs`)

Manages the segment files on disk, enforcing storage limits.

```rust
pub struct RingBuffer {
    /// Segment directory
    dir: PathBuf,
    /// Ordered list of segment IDs currently on disk (oldest first)
    segments: Vec<SegmentInfo>,
    /// Channel to emit eviction events
    event_tx: mpsc::Sender<SegmentEvent>,
}

struct SegmentInfo {
    segment_id: String,
    start_ts: u64,
    end_ts: u64,
    size_bytes: u64,
    path: PathBuf,
}

impl RingBuffer {
    /// Scan the segment directory and rebuild state from files on disk.
    pub async fn scan(dir: &Path, event_tx: mpsc::Sender<SegmentEvent>) -> Result<Self>;

    /// Register a newly finalized segment.
    pub fn register(&mut self, info: SegmentInfo);

    /// Evict oldest segments until at least `needed_bytes` of free space is available.
    /// Returns the evicted segment IDs.
    pub async fn ensure_space(&mut self, needed_bytes: u64) -> Result<Vec<String>>;

    /// Emergency eviction: delete the oldest N segments.
    pub async fn emergency_evict(&mut self, count: usize) -> Result<Vec<String>>;

    /// Get the full ordered list of segments (for manifest generation).
    pub fn segments(&self) -> &[SegmentInfo];

    /// Look up a segment by ID. Returns the file path if it exists.
    pub fn get_segment_path(&self, segment_id: &str) -> Option<&Path>;

    /// Get the init segment path.
    pub fn init_path(&self) -> PathBuf;

    /// Check available space on the data partition.
    pub async fn available_space(&self) -> Result<u64>;
}
```

### 3.5 Manifest Generator (`camera/src/recording/manifest.rs`)

```rust
/// Generate an HLS v7 manifest from the current ring buffer contents.
///
/// Format:
/// ```m3u8
/// #EXTM3U
/// #EXT-X-VERSION:7
/// #EXT-X-TARGETDURATION:10
/// #EXT-X-MAP:URI="init.mp4"
/// #EXTINF:10.0,
/// {segment_id}.m4s
/// ...
/// ```
///
/// No #EXT-X-ENDLIST tag — manifest is open-ended (rolling buffer).
pub fn generate_manifest(segments: &[SegmentInfo]) -> String;

/// Write manifest to disk at the given path.
pub async fn write_manifest(path: &Path, manifest: &str) -> Result<()>;
```

### 3.6 Upload Handler (`camera/src/recording/uploads.rs`)

Handles upload commands from the server. Integrates with the session from Plan 7.

```rust
/// Run the upload handler loop. Receives upload commands from the command
/// handler and opens QUIC upload streams.
pub async fn run_upload_handler(
    connection: quinn::Connection,
    cmd_rx: mpsc::Receiver<Command>,
    ring_buffer: &RingBuffer,
    init_segment: &RwLock<Option<Bytes>>,
    alerts_tx: &Mutex<SendStream>,
    cancel: CancellationToken,
) -> Result<()> {
    loop {
        let cmd = cmd_rx.recv().await?;
        match cmd {
            Command::UploadSegment { seq, segment_id } => {
                handle_upload_segment(
                    &connection, ring_buffer, alerts_tx, seq, &segment_id
                ).await;
            }
            Command::UploadInit { seq } => {
                handle_upload_init(
                    &connection, init_segment, seq
                ).await;
            }
            _ => {}
        }
    }
}

/// Upload a segment file to the server.
///
/// 1. Look up segment in ring buffer
/// 2. If not found: send segment_upload_failed alert (reason: evicted or not_found)
/// 3. Open QUIC uni stream with type tag 0x00
/// 4. Read file and write raw bytes
/// 5. Close stream
/// 6. Send segment_uploaded alert
async fn handle_upload_segment(
    connection: &quinn::Connection,
    ring_buffer: &RingBuffer,
    alerts_tx: &Mutex<SendStream>,
    seq: u64,
    segment_id: &str,
);

/// Upload the current init segment to the server.
///
/// 1. Open QUIC uni stream with type tag 0x01
/// 2. Write init segment bytes
/// 3. Close stream
/// No alert on completion — server infers from stream close.
async fn handle_upload_init(
    connection: &quinn::Connection,
    init_segment: &RwLock<Option<Bytes>>,
    seq: u64,
);
```

### 3.7 Manifest Push (`camera/src/recording/manifest_push.rs`)

Camera-initiated: pushes the manifest to the server after each update.

```rust
/// Push the current manifest to the server via a QUIC upload stream.
///
/// 1. Open QUIC uni stream with type tag 0x02
/// 2. Write UTF-8 manifest bytes
/// 3. Close stream
pub async fn push_manifest(
    connection: &quinn::Connection,
    manifest: &str,
) -> Result<()>;
```

### 3.8 Storage-Full Handling (`camera/src/recording/storage.rs`)

```rust
/// Handle ENOSPC during segment write.
///
/// 1. Attempt emergency eviction (oldest 5 segments)
/// 2. Retry the write
/// 3. If still ENOSPC: pause recording, emit StorageFull event
/// 4. Start polling free space every 60s
/// 5. If free space becomes available: resume recording, emit StorageResumed
pub async fn handle_storage_full(
    ring_buffer: &mut RingBuffer,
    event_tx: &mpsc::Sender<SegmentEvent>,
) -> Result<StorageAction>;

pub enum StorageAction {
    /// Space recovered, continue recording
    Recovered,
    /// Recording paused, polling for space
    Paused,
}

/// Poll free space in the background while recording is paused.
pub async fn poll_for_space(
    ring_buffer: &RingBuffer,
    event_tx: &mpsc::Sender<SegmentEvent>,
    cancel: CancellationToken,
) -> Result<()>;
```

### 3.9 Startup Recovery (`camera/src/recording/recovery.rs`)

```rust
/// Scan the segment directory and recover state after a crash.
///
/// 1. List all .m4s files in segment_dir
/// 2. Attempt to parse each as valid fMP4
/// 3. Delete files that cannot be parsed
/// 4. Delete partially written segments (incomplete moof/mdat)
/// 5. Rebuild manifest from surviving segments
/// 6. Return the recovered RingBuffer
pub async fn recover(
    segment_dir: &Path,
    device_id: &str,
    event_tx: mpsc::Sender<SegmentEvent>,
) -> Result<(RingBuffer, String)>;  // (ring_buffer, manifest)
```

### 3.10 Session Integration

Update `Session::establish` (Plan 7) to include recording setup:

```rust
// In session.rs establish():

// After handshake and telemetry buffer upload:

// 1. Push current manifest to server
push_manifest(&connection, &manifest).await?;

// 2. Send recording_segment alerts for segments not yet acknowledged
for segment in ring_buffer.segments() {
    send_alert(&mut alerts_tx, Alert::RecordingSegment {
        device_id: device_id.clone(),
        segment_id: segment.segment_id.clone(),
        start_ts: segment.start_ts,
        end_ts: segment.end_ts,
        size_bytes: segment.size_bytes,
    }).await?;
}
```

Update `Session::run` to spawn the upload handler and wire segment events to alerts:

```rust
// In session.rs run():

// Spawn: segment_event_forwarder
// Reads SegmentEvent from muxer, sends corresponding alerts + manifest pushes:
//   Finalized → send recording_segment alert + push manifest
//   Evicted → send segment_evicted alert + push manifest
//   StorageFull → send storage_full alert
//   StorageResumed → send storage_resumed alert

// Spawn: upload_handler
// Receives UploadSegment/UploadInit commands, opens QUIC streams
```

### 3.11 Capture Fan-Out Update

Update `start_capture` (Plan 7) to provide separate receivers for the QUIC stream writers and the muxer:

```rust
/// Start the capture pipeline. Returns receivers for:
/// - stream_rx: consumed by QUIC video/audio stream writers
/// - muxer_video_rx: consumed by the fMP4 muxer (video NALs)
/// - muxer_audio_rx: consumed by the fMP4 muxer (audio frames)
///
/// Internally uses broadcast channels so both consumers get every frame.
pub async fn start_capture(
    config: &CameraConfig,
    cancel: CancellationToken,
) -> Result<(CaptureReceiver, broadcast::Receiver<Bytes>, broadcast::Receiver<Bytes>)>;
```

### 3.12 Module Structure

```
camera/src/
├── recording/
│   ├── mod.rs              # Re-exports, recording orchestration
│   ├── muxer.rs            # fMP4 muxer task
│   ├── segment.rs          # SegmentWriter (single .m4s file)
│   ├── init.rs             # Init segment generator
│   ├── ring_buffer.rs      # Ring buffer management + eviction
│   ├── manifest.rs         # HLS manifest generation
│   ├── manifest_push.rs    # QUIC manifest push
│   ├── uploads.rs          # Segment + init upload handler
│   ├── storage.rs          # Storage-full handling
│   └── recovery.rs         # Crash recovery / startup scan
```

---

## 4. Testing Plan

### 4.1 Unit Tests — Init Segment

**Location:** `camera/src/recording/init.rs`

| Test | Description |
|------|-------------|
| `generate_init_from_sps_pps` | Provide SPS + PPS bytes → produces non-empty init segment |
| `init_starts_with_ftyp` | Output begins with `ftyp` box |
| `init_contains_moov` | Output contains `moov` box |
| `init_contains_video_track` | moov contains a video trak with avc1 sample entry |
| `init_contains_audio_track` | moov contains an audio trak with Opus sample entry |
| `init_contains_mvex` | moov contains mvex with trex entries |

### 4.2 Unit Tests — Manifest Generation

**Location:** `camera/src/recording/manifest.rs`

| Test | Description |
|------|-------------|
| `empty_segments` | No segments → minimal valid manifest with headers only |
| `single_segment` | 1 segment → manifest has 1 EXTINF entry |
| `multiple_segments` | 5 segments → 5 EXTINF entries in order |
| `manifest_has_version_7` | Output contains `#EXT-X-VERSION:7` |
| `manifest_has_target_duration` | Output contains `#EXT-X-TARGETDURATION:10` |
| `manifest_has_init_map` | Output contains `#EXT-X-MAP:URI="init.mp4"` |
| `manifest_no_endlist` | Output does NOT contain `#EXT-X-ENDLIST` |
| `segment_ids_as_filenames` | Each EXTINF is followed by `{segment_id}.m4s` |

### 4.3 Unit Tests — Ring Buffer

**Location:** `camera/src/recording/ring_buffer.rs`

| Test | Description |
|------|-------------|
| `scan_empty_dir` | Empty directory → empty buffer |
| `scan_existing_segments` | Directory with 3 .m4s files → 3 segments in order |
| `scan_ignores_non_m4s` | Directory with .txt and .m4s files → only .m4s loaded |
| `register_adds_segment` | Register a segment → appears in segments() |
| `ensure_space_no_eviction` | Plenty of space → no eviction, empty vec returned |
| `ensure_space_evicts_oldest` | Not enough space → oldest segment evicted, file deleted |
| `ensure_space_evicts_multiple` | Need to evict 3 to make room → 3 oldest evicted |
| `emergency_evict` | Emergency evict 5 → 5 oldest deleted (or all if < 5) |
| `get_segment_path_found` | Registered segment → returns path |
| `get_segment_path_not_found` | Unknown segment → None |

### 4.4 Unit Tests — Segment Writer

**Location:** `camera/src/recording/segment.rs`

| Test | Description |
|------|-------------|
| `create_segment_file` | Create → file exists on disk |
| `write_video_increases_size` | Write video data → size increases |
| `write_audio_increases_size` | Write audio data → size increases |
| `finalize_returns_metadata` | Finalize → metadata has correct segment_id, start_ts, non-zero size |
| `finalized_file_is_valid` | Finalized segment file can be parsed as fMP4 (contains moof + mdat) |

### 4.5 Unit Tests — Storage Handling

**Location:** `camera/src/recording/storage.rs`

| Test | Description |
|------|-------------|
| `emergency_eviction_recovers` | Simulate low space, call handle_storage_full → evicts segments, returns Recovered |
| `storage_full_pauses` | Simulate no space even after eviction → returns Paused, StorageFull event emitted |

### 4.6 Unit Tests — Upload Handler

**Location:** `camera/src/recording/uploads.rs`

| Test | Description |
|------|-------------|
| `upload_segment_opens_stream` | Send UploadSegment command → QUIC stream opened with tag 0x00 |
| `upload_segment_writes_file` | → File contents match segment on disk |
| `upload_segment_sends_uploaded_alert` | → segment_uploaded alert sent with correct seq + segment_id |
| `upload_segment_not_found` | Segment evicted → segment_upload_failed alert with reason "evicted" |
| `upload_init_opens_stream` | Send UploadInit → QUIC stream opened with tag 0x01 |
| `upload_init_writes_data` | → Init segment bytes written |

### 4.7 Unit Tests — Manifest Push

**Location:** `camera/src/recording/manifest_push.rs`

| Test | Description |
|------|-------------|
| `push_opens_stream_with_tag` | Push manifest → QUIC stream opened with tag 0x02 |
| `push_writes_utf8` | → Stream contains UTF-8 manifest string |

### 4.8 Unit Tests — Crash Recovery

**Location:** `camera/src/recording/recovery.rs`

| Test | Description |
|------|-------------|
| `recover_empty_dir` | Empty dir → empty ring buffer, empty manifest |
| `recover_valid_segments` | Dir with 3 valid segments → all recovered, manifest has 3 entries |
| `recover_deletes_corrupt` | Dir with 2 valid + 1 truncated → truncated deleted, 2 recovered |
| `recover_rebuilds_manifest` | Recovered manifest matches segments on disk |

### 4.9 Integration Tests — Muxer Pipeline

**Location:** `camera/tests/recording_integration.rs`

| Test | Description |
|------|-------------|
| `muxer_creates_segments` | Feed 30s of test H.264 frames → 3 segments created on disk (~10s each) |
| `muxer_creates_init` | Feed first IDR → InitReady event emitted with non-empty data |
| `muxer_emits_finalized_events` | → 3 Finalized events with correct start_ts, end_ts, size_bytes |
| `muxer_segment_ids` | Segment IDs follow `{device_id}:{start_ts}` format |
| `muxer_gop_alignment` | Each segment starts with an IDR NAL |
| `muxer_audio_muxed` | Feed video + audio → segment files contain both tracks |
| `segments_independently_decodable` | Each segment (after init) can be parsed as valid fMP4 |
| `manifest_updates_per_segment` | ManifestUpdated event after each segment finalization |
| `ring_buffer_eviction` | Fill a small test dir (limit to 5 segments), continue recording → oldest evicted, Evicted events emitted |
| `manifest_reflects_eviction` | After eviction, manifest no longer lists evicted segment |

### 4.10 Integration Tests — Upload End-to-End

**Location:** `camera/tests/upload_integration.rs`

Uses MockServer from Plan 7.

| Test | Description |
|------|-------------|
| `upload_segment_e2e` | Record segments, server sends upload_segment command → server receives segment bytes via upload stream, camera sends segment_uploaded alert |
| `upload_init_e2e` | Server sends upload_init → server receives init bytes via upload stream |
| `manifest_pushed_on_connect` | Camera connects → server receives manifest push stream (tag 0x02) with current manifest |
| `manifest_pushed_on_segment` | Camera records a segment → server receives updated manifest push |
| `recording_segment_alert_on_connect` | Camera has 3 existing segments, connects → server receives 3 recording_segment alerts |
| `upload_evicted_segment` | Camera records, server requests old segment that was evicted → camera sends segment_upload_failed |

### 4.11 Integration Tests — Storage Full

**Location:** `camera/tests/storage_integration.rs`

| Test | Description |
|------|-------------|
| `storage_full_alert` | Use a tiny tmpfs/limited dir, record until full → storage_full alert sent |
| `storage_resumed_after_free` | After storage_full, delete files externally → storage_resumed alert sent, recording resumes |
| `live_streaming_unaffected` | Storage full → video/audio streams continue (only recording pauses) |

### 4.12 Build Validation

| Check | Command | Expected |
|-------|---------|----------|
| Camera compiles | `cargo build -p camera` | Succeeds |
| Unit tests | `cargo test -p camera` (non-ignored) | Pass |
| Integration tests | `cargo test -p camera -- --ignored` | Pass |
| Segment files valid | Feed test H.264 through muxer, open output in ffprobe | Recognized as fMP4 with H.264 + Opus |
| Clippy | `cargo clippy -p camera -- -D warnings` | Clean |
| Format | `cargo fmt --check` | Clean |

---

## 5. Validation Checklist

After completing this plan, verify:

- [ ] `cargo build -p camera` succeeds
- [ ] `cargo test -p camera` passes all tests
- [ ] Muxer produces valid fMP4 segments parseable by ffprobe
- [ ] Init segment contains correct SPS/PPS and Opus codec parameters
- [ ] Segments are GOP-aligned (start with IDR)
- [ ] Target segment duration is ~10 seconds
- [ ] Ring buffer fills available space, evicts oldest segments
- [ ] Evicted segments are deleted from disk
- [ ] HLS manifest is valid M3U8 v7 format
- [ ] Manifest updates on each segment finalization and eviction
- [ ] Manifest has no `#EXT-X-ENDLIST` tag
- [ ] Manifest push sends to server via QUIC stream with tag 0x02
- [ ] upload_segment reads file and sends via QUIC stream with tag 0x00
- [ ] upload_init sends init segment via QUIC stream with tag 0x01
- [ ] segment_uploaded alert sent after successful upload
- [ ] segment_upload_failed alert sent for evicted/missing segments
- [ ] recording_segment alerts sent on connect for existing segments
- [ ] Storage-full condition pauses recording, sends alert
- [ ] Storage-resumed condition resumes recording, sends alert
- [ ] Live streaming is unaffected by storage-full
- [ ] Crash recovery scans directory, deletes corrupt files, rebuilds manifest
- [ ] Capture pipeline fans out to both QUIC stream writers and muxer
- [ ] `CLAUDE.md` updated with recording module structure
