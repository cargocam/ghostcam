# Plan 7: Camera Firmware — Core Connection & Streaming

**Status:** Not started
**Branch:** `rewrite`
**Depends on:** Plan 1 (shared types)
**Unlocks:** Plan 8 (fMP4 recording), Plan 9 (enrollment/QR/OTA)

---

## 1. Goal

Implement the camera firmware's core runtime: QUIC connection with mTLS, the handshake and alert/command channel protocol, video and audio stream writing (gated on server commands), telemetry datagram loop with threshold-based diffing and offline buffering, reconnection with exponential backoff, and test-source mode for development without Pi hardware.

After this plan, the camera binary can connect to the server, complete the handshake, send video/audio frames when commanded, transmit telemetry as QUIC datagrams, handle commands, buffer telemetry offline, and upload the buffer on reconnect. Recording (Plan 8), enrollment (Plan 9), and OTA (Plan 9) come later.

---

## 2. Crate Changes

### 2.1 Dependencies

**`camera/Cargo.toml`**:
```toml
[dependencies]
ghostcam = { path = "../ghostcam" }
quinn.workspace = true
rustls.workspace = true
rcgen.workspace = true
tokio = { workspace = true, features = ["full"] }
tokio-util.workspace = true
bytes.workspace = true
anyhow.workspace = true
tracing.workspace = true
tracing-subscriber = "0.3"
serde.workspace = true
serde_json.workspace = true
rmp-serde.workspace = true
clap = { version = "4", features = ["derive"] }

[target.'cfg(target_os = "linux")'.dependencies]
cpal = "0.15"
opus = "0.3"

[dev-dependencies]
tokio = { workspace = true, features = ["full", "test-util"] }
tempfile = "3"
```

Audio dependencies (`cpal`, `opus`) are Linux-only. On macOS/other platforms, audio capture is unavailable and the camera uses silence frames (same as `--test-source`).

---

## 3. Implementation Details

### 3.1 CLI (`camera/src/main.rs`)

```rust
#[derive(Parser)]
#[command(name = "ghostcam-camera")]
struct Cli {
    /// Server QUIC address (overrides all other sources)
    #[arg(long)]
    server_addr: Option<String>,

    /// Use test video + audio sources instead of real capture
    #[arg(long)]
    test_source: bool,

    /// Path to test H.264 file (default: test-data/test.h264)
    #[arg(long, default_value = "test-data/test.h264")]
    test_video: String,

    /// Directory for fMP4 ring buffer (default: /var/ghostcam/segments)
    #[arg(long, default_value = "/var/ghostcam/segments")]
    segment_dir: String,

    /// Disable audio capture
    #[arg(long)]
    no_audio: bool,

    /// Disable GPS even if gpsd is available
    #[arg(long)]
    no_gps: bool,

    /// Data directory (default: /var/ghostcam)
    #[arg(long, default_value = "/var/ghostcam")]
    data_dir: String,
}
```

Main function:
1. Parse CLI args
2. Read `ghostcam.conf` from `/boot/ghostcam.conf` if present (merge with CLI)
3. Load device cert and user association cert from disk
4. If no user association cert → enter registration mode (stub in this plan, Plan 9 implements)
5. Resolve server address (CLI → ghostcam.conf → server.addr file → cloud default)
6. Start capture pipeline
7. Enter connection loop (connect → run → reconnect on failure)
8. Handle SIGTERM/SIGINT for clean shutdown

### 3.2 Configuration (`camera/src/config.rs`)

```rust
/// Parsed camera configuration from all sources.
pub struct CameraConfig {
    pub server_addr: String,
    pub test_source: bool,
    pub test_video: String,
    pub segment_dir: String,
    pub no_audio: bool,
    pub no_gps: bool,
    pub data_dir: String,
}

/// Parse ghostcam.conf (TOML) from the boot partition.
pub fn read_ghostcam_conf(path: &Path) -> Result<Option<GhostcamConf>>;

#[derive(Deserialize)]
pub struct GhostcamConf {
    pub server_addr: Option<String>,
    #[serde(default)]
    pub no_audio: bool,
    #[serde(default)]
    pub no_gps: bool,
}

/// Resolve server address with precedence:
/// 1. CLI --server-addr
/// 2. ghostcam.conf server_addr
/// 3. /etc/ghostcam/server.addr (stored during enrollment)
/// 4. Hardcoded cloud default
pub fn resolve_server_addr(
    cli: Option<&str>,
    conf: Option<&str>,
    addr_file: &Path,
) -> String;
```

### 3.3 Certificate Loading (`camera/src/certs.rs`)

```rust
/// Load the device certificate and key from disk.
/// If they don't exist, generate new ones (first boot).
pub fn load_or_create_device_cert(
    cert_path: &Path,
    key_path: &Path,
) -> Result<(Vec<u8>, Vec<u8>)>;  // (cert_der, key_der)

/// Load the user association certificate and key from disk.
/// Returns None if not enrolled.
pub fn load_user_cert(
    cert_path: &Path,
    key_path: &Path,
) -> Result<Option<(Vec<u8>, Vec<u8>)>>;  // (cert_der, key_der)

/// Load the CA certificate from disk (for server verification).
/// Returns None if not enrolled.
pub fn load_ca_cert(path: &Path) -> Result<Option<Vec<u8>>>;

/// Load the server TLS fingerprint pin (server-solo TOFU).
/// Returns None if not present.
pub fn load_server_pin(path: &Path) -> Result<Option<String>>;
```

### 3.4 QUIC Client (`camera/src/quic.rs`)

```rust
/// Build a Quinn client endpoint with mTLS.
///
/// - device_cert + device_key: always presented
/// - user_cert + user_key: presented if enrolled (None for enrollment connections)
/// - ca_cert: for verifying server's user association cert signing (if present)
/// - server_pin: SHA-256 fingerprint for TOFU verification (server-solo)
///
/// Server TLS verification:
/// - If server_pin is set: custom verifier checks server cert fingerprint
/// - If ca_cert is set but no pin: standard verification against CA (server-multi)
/// - If neither: accept any server cert (enrollment mode)
pub fn build_client_endpoint(
    device_cert_der: &[u8],
    device_key_der: &[u8],
    user_cert_der: Option<&[u8]>,
    user_key_der: Option<&[u8]>,
    server_pin: Option<&str>,
) -> Result<quinn::Endpoint>;

/// Connect to the server. Returns the QUIC connection.
pub async fn connect(
    endpoint: &quinn::Endpoint,
    server_addr: &str,
) -> Result<quinn::Connection>;
```

### 3.5 Connection Session (`camera/src/session.rs`)

Manages a single QUIC connection session. Owns all stream read/write loops.

```rust
pub struct Session {
    connection: quinn::Connection,
    /// Alerts stream (camera → server)
    alerts_tx: SendStream,
    /// Video stream (camera → server)
    video_tx: SendStream,
    /// Audio stream (camera → server)
    audio_tx: SendStream,
    /// Commands stream (server → camera)
    commands_rx: RecvStream,
    /// Video streaming enabled (toggled by start/stop_video commands)
    video_enabled: Arc<AtomicBool>,
    /// Audio streaming enabled
    audio_enabled: Arc<AtomicBool>,
    /// Cancellation
    cancel: CancellationToken,
}

impl Session {
    /// Establish a session:
    /// 1. Open 3 outbound uni streams (Alerts, Video, Audio)
    /// 2. Accept 1 inbound uni stream (Commands)
    /// 3. Send handshake alert
    /// 4. Send networks alert (current known WiFi networks)
    /// 5. If telemetry buffer has entries, open upload stream and flush
    /// 6. Return the session
    pub async fn establish(
        connection: quinn::Connection,
        config: &CameraConfig,
        telemetry_buffer: &TelemetryBuffer,
    ) -> Result<Self>;

    /// Run the session. Spawns concurrent tasks and waits for any to fail.
    ///
    /// Tasks:
    /// - command_reader: reads Commands stream, dispatches
    /// - video_writer: reads from capture channel, writes to Video stream (gated)
    /// - audio_writer: reads from capture channel, writes to Audio stream (gated)
    /// - telemetry_sender: reads from telemetry channel, sends QUIC datagrams
    /// - upload_handler: handles upload commands (segment, init, manifest — Plan 8)
    ///
    /// Returns when any task fails or the cancel token fires.
    pub async fn run(
        self,
        capture_rx: CaptureReceiver,
        telemetry_rx: mpsc::Receiver<TelemetryDatagram>,
    ) -> Result<()>;
}
```

### 3.6 Command Handler (`camera/src/commands.rs`)

```rust
/// Read commands from the Commands stream and dispatch.
pub async fn run_command_reader(
    commands_rx: &mut RecvStream,
    video_enabled: Arc<AtomicBool>,
    audio_enabled: Arc<AtomicBool>,
    alerts_tx: &Mutex<SendStream>,
    upload_cmd_tx: mpsc::Sender<Command>,
) -> Result<()> {
    loop {
        let cmd: Command = ghostcam::wire::framing::read_json(commands_rx).await?
            .ok_or_else(|| anyhow::anyhow!("commands stream closed"))?;

        match cmd {
            Command::StartVideo { .. } => {
                video_enabled.store(true, Ordering::SeqCst);
                tracing::info!("video streaming enabled");
            }
            Command::StopVideo { .. } => {
                video_enabled.store(false, Ordering::SeqCst);
                tracing::info!("video streaming disabled");
            }
            Command::StartAudio { .. } => {
                audio_enabled.store(true, Ordering::SeqCst);
                tracing::info!("audio streaming enabled");
            }
            Command::StopAudio { .. } => {
                audio_enabled.store(false, Ordering::SeqCst);
                tracing::info!("audio streaming disabled");
            }
            Command::Reboot { seq } => {
                send_ack(alerts_tx, "reboot", seq).await?;
                tracing::info!("reboot requested");
                #[cfg(target_os = "linux")]
                std::process::Command::new("systemctl").arg("reboot").spawn()?;
                #[cfg(not(target_os = "linux"))]
                tracing::warn!("reboot not supported on this platform");
            }
            // Upload commands forwarded to upload handler (Plan 8)
            cmd @ (Command::UploadSegment { .. }
                | Command::UploadInit { .. }) => {
                upload_cmd_tx.send(cmd).await?;
            }
            // Network commands (Plan 9)
            Command::NetworkConfig { .. }
            | Command::RemoveNetwork { .. }
            | Command::ListNetworks { .. } => {
                tracing::info!(?cmd, "network command — not yet implemented");
            }
            // OTA commands (Plan 9)
            Command::UpdateAvailable { seq, .. } => {
                send_ack(alerts_tx, "update_available", seq).await?;
                tracing::info!("update_available — not yet implemented");
            }
            // Certificate commands (Plan 9)
            Command::CertRefresh { seq, .. } => {
                send_ack(alerts_tx, "cert_refresh", seq).await?;
                tracing::info!("cert_refresh — not yet implemented");
            }
            Command::Unregister { seq } => {
                send_ack(alerts_tx, "unregister", seq).await?;
                tracing::info!("unregister — not yet implemented");
            }
        }
    }
}

/// Send an ack alert on the Alerts stream.
async fn send_ack(
    alerts_tx: &Mutex<SendStream>,
    cmd: &str,
    seq: u64,
) -> Result<()>;
```

### 3.7 Capture Pipeline (`camera/src/capture/mod.rs`)

The capture pipeline produces frames independently of the QUIC connection. Frames are sent via a channel; the session tasks consume them.

```rust
/// Messages produced by the capture pipeline.
pub enum CaptureMessage {
    /// H.264 NAL unit(s) from video capture.
    VideoNal(Bytes),
    /// Opus-encoded audio frame.
    AudioFrame(Bytes),
}

pub type CaptureSender = mpsc::Sender<CaptureMessage>;
pub type CaptureReceiver = mpsc::Receiver<CaptureMessage>;

/// Start the capture pipeline. Returns a receiver for capture messages.
/// The pipeline runs until the cancel token fires.
pub async fn start_capture(
    config: &CameraConfig,
    cancel: CancellationToken,
) -> Result<CaptureReceiver>;
```

### 3.8 Video Capture — Test Source (`camera/src/capture/video_test.rs`)

```rust
/// Loop a test H.264 file, emitting NAL units at ~30fps.
///
/// Reads the file, parses Annex-B NAL units, sends each via the channel
/// with appropriate timing delays.
pub async fn run_test_video(
    path: &str,
    tx: CaptureSender,
    cancel: CancellationToken,
) -> Result<()>;
```

### 3.9 Video Capture — Real (`camera/src/capture/video_real.rs`)

```rust
/// Launch rpicam-vid as a subprocess and stream H.264 NAL units.
///
/// rpicam-vid args:
///   --codec h264 --profile baseline
///   --width 640 --height 480 --framerate 30
///   --keyint 300 (10s at 30fps for segment alignment)
///   --output - (pipe to stdout)
///   --timeout 0 (run indefinitely)
///
/// Reads stdout, parses Annex-B boundaries, sends NAL units via channel.
#[cfg(target_os = "linux")]
pub async fn run_real_video(
    tx: CaptureSender,
    cancel: CancellationToken,
) -> Result<()>;
```

### 3.10 Audio Capture — Test Source (`camera/src/capture/audio_test.rs`)

```rust
/// Send 3-byte Opus silence frames every 20ms.
pub async fn run_test_audio(
    tx: CaptureSender,
    cancel: CancellationToken,
) -> Result<()>;

const OPUS_SILENCE: [u8; 3] = [0xF8, 0xFF, 0xFE];
```

### 3.11 Audio Capture — Real (`camera/src/capture/audio_real.rs`)

```rust
/// Open cpal audio input, read PCM, encode Opus, send frames.
///
/// PCM: mono, 48kHz, f32
/// Opus: 48kHz, mono, 20ms frames (960 samples)
/// Resampling from device native rate if needed.
#[cfg(target_os = "linux")]
pub async fn run_real_audio(
    tx: CaptureSender,
    cancel: CancellationToken,
) -> Result<()>;
```

### 3.12 Video Stream Writer (`camera/src/stream/video.rs`)

```rust
/// Read video NAL units from the capture channel and write them to the
/// QUIC Video stream as length-prefixed frames.
///
/// Gated on `video_enabled` — when false, frames are consumed from the
/// channel but not written to the stream (keeps the channel drained).
/// The capture pipeline and fMP4 muxer (Plan 8) continue regardless.
pub async fn run_video_writer(
    video_tx: &mut SendStream,
    capture_rx: &mut CaptureReceiver,
    video_enabled: Arc<AtomicBool>,
    cancel: CancellationToken,
) -> Result<()>;
```

Note: `CaptureReceiver` is shared between the video writer and the fMP4 muxer (Plan 8). In practice, the capture pipeline fans out via broadcast or multiple mpsc channels. The video writer consumes one copy; the muxer consumes another. This fan-out is set up in `start_capture`.

### 3.13 Audio Stream Writer (`camera/src/stream/audio.rs`)

```rust
/// Same pattern as video: read Opus frames, write length-prefixed to Audio stream.
/// Gated on `audio_enabled`.
pub async fn run_audio_writer(
    audio_tx: &mut SendStream,
    capture_rx: &mut CaptureReceiver,
    audio_enabled: Arc<AtomicBool>,
    cancel: CancellationToken,
) -> Result<()>;
```

### 3.14 Telemetry Loop (`camera/src/telemetry/mod.rs`)

```rust
/// Run the telemetry poll-and-send loop.
///
/// Every 2 seconds:
/// 1. Poll all sensors (sensor_reader)
/// 2. Compute threshold diff against previous payload
/// 3. If any threshold exceeded OR 30s heartbeat due:
///    a. If QUIC connected: send as QUIC datagram (MessagePack)
///    b. If QUIC disconnected: write to telemetry buffer
///
/// The loop runs independently of the QUIC connection.
pub async fn run_telemetry_loop(
    connection: Option<&quinn::Connection>,  // None when disconnected
    buffer: &TelemetryBuffer,
    config: &CameraConfig,
    cancel: CancellationToken,
) -> Result<()>;
```

In practice, the telemetry loop always runs. It receives a watch channel indicating whether the QUIC connection is available. When connected, it sends datagrams directly. When disconnected, it writes to the buffer.

### 3.15 Sensor Reader (`camera/src/telemetry/sensors.rs`)

```rust
/// Read all available sensor values.
pub async fn read_sensors(config: &CameraConfig) -> TelemetryDatagram;
```

Platform-specific implementations:

```rust
#[cfg(target_os = "linux")]
mod linux {
    /// Read CPU usage from /proc/stat
    pub fn read_cpu() -> Option<u32>;

    /// Read memory usage from /proc/meminfo
    pub fn read_memory() -> Option<u32>;

    /// Read SoC temperature from /sys/class/thermal/thermal_zone0/temp
    pub fn read_temperature() -> Option<u32>;

    /// Read system uptime from /proc/uptime
    pub fn read_uptime() -> Option<u32>;

    /// Read WiFi signal strength via nl80211 or iwconfig
    pub fn read_wifi_signal() -> Option<i8>;
}

#[cfg(not(target_os = "linux"))]
mod fallback {
    /// Return synthetic test values for development on non-Linux.
    pub fn read_cpu() -> Option<u32> { Some(15) }
    pub fn read_memory() -> Option<u32> { Some(256) }
    pub fn read_temperature() -> Option<u32> { Some(45) }
    pub fn read_uptime() -> Option<u32> { /* real uptime via std */ }
    pub fn read_wifi_signal() -> Option<i8> { Some(-55) }
}
```

### 3.16 GPS Reader (`camera/src/telemetry/gps.rs`)

```rust
/// Read GPS data from gpsd shared memory interface.
/// Returns None if gpsd is unavailable or no fix.
///
/// Only attempted on Linux when --no-gps is not set.
/// Fails silently (returns None) if gpsd socket is absent.
#[cfg(target_os = "linux")]
pub async fn read_gps(no_gps: bool) -> Option<GpsReading>;

#[cfg(not(target_os = "linux"))]
pub async fn read_gps(_no_gps: bool) -> Option<GpsReading> { None }

pub struct GpsReading {
    pub lat: f64,
    pub lon: f64,
    pub alt: f32,
    pub fix: u8,
}
```

### 3.17 Telemetry Buffer (`camera/src/telemetry/buffer.rs`)

On-disk buffer for telemetry datagrams generated while the QUIC connection is unavailable.

```rust
pub struct TelemetryBuffer {
    /// In-memory buffer (flushed to disk periodically and on shutdown)
    entries: RwLock<Vec<TelemetryDatagram>>,
    /// Path to the on-disk buffer file
    path: PathBuf,
    /// Maximum entries (100,000)
    cap: usize,
}

impl TelemetryBuffer {
    /// Load the buffer from disk (or create empty if file doesn't exist).
    pub fn load(path: &Path) -> Result<Self>;

    /// Append a datagram with deduplication.
    ///
    /// Dedup algorithm:
    /// if new == entries[-1]:
    ///     if new == entries[-2]:
    ///         entries[-1].ts = new.ts  // update timestamp in place
    ///     else:
    ///         entries.push(new)
    /// else:
    ///     entries.push(new)
    ///
    /// If at cap, evict oldest entries.
    pub async fn push(&self, datagram: TelemetryDatagram);

    /// Drain all entries for upload. Returns the entries and clears the buffer.
    pub async fn drain(&self) -> Vec<TelemetryDatagram>;

    /// Check if the buffer has entries.
    pub async fn is_empty(&self) -> bool;

    /// Flush in-memory buffer to disk.
    pub async fn flush_to_disk(&self) -> Result<()>;

    /// Clear the on-disk buffer (after successful upload).
    pub async fn clear_disk(&self) -> Result<()>;

    /// Entry count.
    pub async fn len(&self) -> usize;
}
```

The on-disk format is a MessagePack-encoded array of `TelemetryDatagram`. The buffer is loaded into memory at startup, appended to in memory, and flushed to disk periodically (every 30s) and on shutdown.

### 3.18 Telemetry Buffer Upload (`camera/src/session.rs`)

During session establishment (after handshake), if the buffer has entries:

```rust
async fn upload_telemetry_buffer(
    connection: &quinn::Connection,
    buffer: &TelemetryBuffer,
) -> Result<()> {
    if buffer.is_empty().await { return Ok(()); }

    let entries = buffer.drain().await;
    let encoded = ghostcam::telemetry::encode_array(&entries)?;

    // Open upload stream with type tag
    let mut stream = connection.open_uni().await?;
    stream.write_all(&[UploadStreamType::TelemetryBuffer as u8]).await?;
    stream.write_all(&encoded).await?;
    stream.finish()?;

    buffer.clear_disk().await?;
    tracing::info!(count = entries.len(), "uploaded telemetry buffer");
    Ok(())
}
```

### 3.19 Connection Loop (`camera/src/main.rs`)

```rust
async fn run_connection_loop(
    config: &CameraConfig,
    capture_rx: CaptureReceiver,
    telemetry_buffer: &TelemetryBuffer,
    cancel: CancellationToken,
) -> Result<()> {
    let mut backoff = Duration::from_secs(RECONNECT_BACKOFF_INITIAL_SECS);

    loop {
        if cancel.is_cancelled() { break; }

        match try_connect_and_run(config, &capture_rx, telemetry_buffer, &cancel).await {
            Ok(()) => {
                // Clean shutdown
                break;
            }
            Err(e) => {
                tracing::warn!("connection lost: {e}");
                tracing::info!("reconnecting in {:?}", backoff);
                tokio::select! {
                    _ = tokio::time::sleep(backoff) => {}
                    _ = cancel.cancelled() => break,
                }
                backoff = (backoff * 2).min(Duration::from_secs(RECONNECT_BACKOFF_MAX_SECS));
            }
        }
    }
    Ok(())
}

async fn try_connect_and_run(
    config: &CameraConfig,
    capture_rx: &CaptureReceiver,
    telemetry_buffer: &TelemetryBuffer,
    cancel: &CancellationToken,
) -> Result<()> {
    let endpoint = build_client_endpoint(...)?;
    let connection = connect(&endpoint, &config.server_addr).await?;
    backoff_reset(); // reset on successful connect

    let session = Session::establish(connection, config, telemetry_buffer).await?;
    session.run(capture_rx, telemetry_rx).await
}
```

### 3.20 Signal Handling (`camera/src/main.rs`)

```rust
async fn shutdown_signal(cancel: CancellationToken) {
    let ctrl_c = tokio::signal::ctrl_c();

    #[cfg(unix)]
    let mut sigterm = tokio::signal::unix::signal(
        tokio::signal::unix::SignalKind::terminate()
    ).unwrap();

    tokio::select! {
        _ = ctrl_c => tracing::info!("SIGINT received"),
        _ = sigterm.recv() => tracing::info!("SIGTERM received"),
    }

    cancel.cancel();
}
```

### 3.21 Module Structure

```
camera/src/
├── main.rs                 # CLI, config resolution, connection loop, shutdown
├── config.rs               # CameraConfig, ghostcam.conf parsing, server addr resolution
├── certs.rs                # Cert loading/generation
├── quic.rs                 # Quinn client endpoint + connect
├── session.rs              # Session establishment + run
├── commands.rs             # Command reader + dispatch
├── capture/
│   ├── mod.rs              # CaptureMessage, start_capture, fan-out
│   ├── video_test.rs       # Test H.264 loop
│   ├── video_real.rs       # rpicam-vid subprocess (Linux only)
│   ├── audio_test.rs       # Opus silence loop
│   └── audio_real.rs       # cpal + Opus encoding (Linux only)
├── stream/
│   ├── mod.rs
│   ├── video.rs            # Video stream writer (gated)
│   └── audio.rs            # Audio stream writer (gated)
└── telemetry/
    ├── mod.rs              # Telemetry loop
    ├── sensors.rs          # Sensor reading (Linux / fallback)
    ├── gps.rs              # GPS reader (gpsd)
    └── buffer.rs           # On-disk telemetry buffer with dedup
```

---

## 4. Testing Plan

### 4.1 Unit Tests — Configuration

**Location:** `camera/src/config.rs`

| Test | Description |
|------|-------------|
| `parse_ghostcam_conf_full` | Parse TOML with server_addr, no_audio, no_gps → correct values |
| `parse_ghostcam_conf_minimal` | Parse TOML with only server_addr → no_audio=false, no_gps=false |
| `parse_ghostcam_conf_empty` | Parse empty TOML → all defaults |
| `parse_ghostcam_conf_missing_file` | File doesn't exist → returns None |
| `resolve_addr_cli_wins` | CLI, conf, and file all set → CLI value used |
| `resolve_addr_conf_second` | CLI None, conf set, file set → conf value used |
| `resolve_addr_file_third` | CLI None, conf None, file set → file value used |
| `resolve_addr_default` | All None → hardcoded cloud default |

### 4.2 Unit Tests — Certificate Loading

**Location:** `camera/src/certs.rs`

| Test | Description |
|------|-------------|
| `create_device_cert_on_first_boot` | No cert files → generates and writes cert + key |
| `load_existing_device_cert` | Create cert, load again → same cert (not regenerated) |
| `load_user_cert_not_enrolled` | No user cert files → returns None |
| `load_user_cert_enrolled` | Write user cert + key → returns Some |

### 4.3 Unit Tests — Command Handler

**Location:** `camera/src/commands.rs`

| Test | Description |
|------|-------------|
| `start_video_enables_flag` | Process StartVideo → video_enabled is true |
| `stop_video_disables_flag` | Process StopVideo → video_enabled is false |
| `start_audio_enables_flag` | Process StartAudio → audio_enabled is true |
| `stop_audio_disables_flag` | Process StopAudio → audio_enabled is false |
| `reboot_sends_ack` | Process Reboot → ack alert sent with cmd="reboot" and matching seq |
| `upload_commands_forwarded` | UploadSegment → forwarded to upload_cmd_tx channel |

### 4.4 Unit Tests — Telemetry Threshold Diffing

Uses the shared `ghostcam::telemetry::exceeds_threshold` function (tested in Plan 1). These tests verify the telemetry loop's integration with diffing.

**Location:** `camera/src/telemetry/mod.rs`

| Test | Description |
|------|-------------|
| `heartbeat_forced_after_30s` | No threshold exceeded for 30s → heartbeat sent anyway |
| `threshold_triggers_immediate_send` | CPU jumps by 10% → datagram sent immediately |
| `no_threshold_no_send` | Values stable within thresholds → no datagram between heartbeats |

### 4.5 Unit Tests — Telemetry Buffer

**Location:** `camera/src/telemetry/buffer.rs`

| Test | Description |
|------|-------------|
| `push_and_drain` | Push 5 entries → drain returns 5, buffer empty |
| `push_dedup_identical_run` | Push 3 identical datagrams → buffer has 2 (first + last with updated ts) |
| `push_dedup_different_breaks_run` | Push A, A, B, B, A → buffer has 4 entries (A, A, B, A) — wait, let me recalculate. A→push. A→equals[-1], not equals[-2](empty), push. B→different, push. B→equals[-1], not equals[-2]=A, push. A→different, push. So 5 entries. Actually: first A pushed (len=1). Second A: equals[-1]=A, len<2 so no [-2] check, push (len=2). B: different from A, push (len=3). B: equals[-1]=B, [-2]=A, different, push (len=4). A: different from B, push (len=5). Hmm, the dedup is only for heartbeat compression. Let me re-test the algorithm properly. |
| `push_dedup_three_identical` | Push A, A, A → buffer has 2 entries (first A and last A with updated ts) |
| `push_dedup_five_identical` | Push A(t=1), A(t=2), A(t=3), A(t=4), A(t=5) → buffer has 2 entries: A(t=1), A(t=5) |
| `push_dedup_change_resets` | Push A, A, A, B, B, B → buffer has 4 entries: A(t=1), A(t=3), B(t=4), B(t=6) |
| `push_respects_cap` | Set cap=10, push 15 → buffer has 10 (oldest 5 evicted) |
| `load_empty` | Load from nonexistent file → empty buffer |
| `flush_and_reload` | Push entries, flush to disk, load from disk → same entries |
| `drain_clears` | Push, drain → is_empty returns true |
| `clear_disk` | Flush to disk, clear_disk → file removed or empty |
| `concurrent_push` | 10 concurrent push tasks → no panic, all entries present |

### 4.6 Unit Tests — Sensor Reader

**Location:** `camera/src/telemetry/sensors.rs`

| Test | Description |
|------|-------------|
| `read_sensors_returns_datagram` | Call read_sensors → returns TelemetryDatagram with ts set |
| `read_sensors_has_cpu` | On any platform (real or fallback) → cpu is Some |
| `read_sensors_has_memory` | → mem is Some |
| `read_sensors_has_uptime` | → uptime is Some |
| `read_sensors_no_gps_when_disabled` | config.no_gps=true → lat/lon/alt/gps_fix are None |

### 4.7 Unit Tests — Capture Pipeline

**Location:** `camera/src/capture/mod.rs`

| Test | Description |
|------|-------------|
| `test_video_produces_frames` | Start test video source → receive at least 10 CaptureMessage::VideoNal within 1s |
| `test_audio_produces_frames` | Start test audio source → receive frames every ~20ms |
| `test_audio_silence_bytes` | Audio frames are 3-byte Opus silence |

### 4.8 Unit Tests — Stream Writers

**Location:** `camera/src/stream/video.rs` and `audio.rs`

| Test | Description |
|------|-------------|
| `video_writer_sends_when_enabled` | video_enabled=true, send capture message → frame appears on stream (use in-memory mock stream) |
| `video_writer_drops_when_disabled` | video_enabled=false, send capture message → nothing written to stream, channel still drained |
| `video_writer_toggle` | Start disabled, send frames (dropped), enable, send frames (written) |
| `audio_writer_sends_when_enabled` | Same pattern for audio |
| `audio_writer_drops_when_disabled` | Same |

### 4.9 Integration Tests — Full Camera Session

**Location:** `camera/tests/session_integration.rs`

These require a running server (from Plans 3-6) or a mock QUIC server. Use a mock QUIC server (mirror of MockCamera but server-side) for isolation.

```rust
/// Mock QUIC server that accepts camera connections and records what it receives.
struct MockServer {
    endpoint: quinn::Endpoint,
    addr: SocketAddr,
}
```

| Test | Description |
|------|-------------|
| `camera_connects_and_handshakes` | Start MockServer, start camera in test-source mode → server receives handshake with protocol_version=1 |
| `camera_sends_video_when_started` | Camera connects, server sends start_video → server receives video frames on Video stream |
| `camera_does_not_send_video_initially` | Camera connects, no start_video sent → no video frames received |
| `camera_stops_video_on_command` | Server sends start_video, waits for frames, sends stop_video → frames stop arriving |
| `camera_sends_audio_when_started` | Same pattern for audio |
| `camera_sends_telemetry_datagrams` | Camera connects → server receives QUIC datagrams, decodable as MessagePack TelemetryDatagram |
| `camera_telemetry_has_required_fields` | Received datagram has ts, cpu, mem, temp, uptime |
| `camera_reconnects_on_disconnect` | Camera connects, server drops connection → camera reconnects within 5s |
| `camera_uploads_telemetry_buffer` | Camera buffers telemetry while disconnected → on reconnect, server receives upload stream (tag=0x03) with buffered entries |
| `camera_responds_to_reboot` | Server sends reboot command → camera sends ack (reboot itself is stubbed on non-Linux) |
| `camera_handles_unknown_commands` | Server sends a command not handled by camera → no crash, logged |
| `camera_shutdown_on_sigterm` | Send SIGTERM → camera shuts down cleanly |

### 4.10 Integration Test — Camera + Real Server

**Location:** `camera/tests/e2e_integration.rs`

Requires the server from Plan 6. One end-to-end test to validate the full path.

| Test | Description |
|------|-------------|
| `camera_to_server_e2e` | Boot server-solo (with test DB), enroll a camera (programmatically via DB), start camera with --test-source → server lists camera as online, video frames flow, telemetry appears in Redis |

### 4.11 Build Validation

| Check | Command | Expected |
|-------|---------|----------|
| Camera compiles | `cargo build -p camera` | Succeeds (macOS or Linux) |
| Camera compiles on Linux | Cross-compile or CI | Succeeds with cpal+opus |
| Unit tests | `cargo test -p camera` | All unit tests pass |
| Integration tests | `cargo test -p camera -- --ignored` | Pass (requires mock server or real server) |
| Camera runs in test mode | `cargo run -p camera -- --test-source --server-addr 127.0.0.1:4433` | Attempts connection (may fail if no server, but doesn't crash) |
| Clippy | `cargo clippy -p camera -- -D warnings` | Clean |
| Format | `cargo fmt --check` | Clean |

---

## 5. Validation Checklist

After completing this plan, verify:

- [ ] `cargo build -p camera` succeeds on macOS and Linux
- [ ] `cargo test -p camera` passes all unit tests
- [ ] Camera binary starts in test-source mode and attempts QUIC connection
- [ ] Camera generates device cert on first boot if not present
- [ ] Camera loads existing device cert on subsequent boots
- [ ] Camera sends handshake alert with correct protocol_version and fw_version
- [ ] Camera opens Alerts, Video, Audio streams and accepts Commands stream
- [ ] start_video/stop_video commands gate video stream writing
- [ ] start_audio/stop_audio commands gate audio stream writing
- [ ] Capture pipeline continues regardless of stream gating
- [ ] Telemetry loop sends QUIC datagrams every 2s (or when thresholds exceeded)
- [ ] Telemetry datagrams are MessagePack-encoded TelemetryDatagram
- [ ] Telemetry heartbeat sent every 30s regardless of thresholds
- [ ] Telemetry buffer stores datagrams when disconnected
- [ ] Telemetry buffer deduplication compresses identical heartbeat runs to 2 entries
- [ ] Telemetry buffer cap (100k) evicts oldest entries
- [ ] Telemetry buffer is uploaded on reconnect via upload stream (tag=0x03)
- [ ] Telemetry buffer is cleared from disk after successful upload
- [ ] Reconnection uses exponential backoff (1s → 30s cap)
- [ ] Backoff resets on successful connection
- [ ] SIGTERM/SIGINT triggers clean shutdown
- [ ] Sensor reading uses real `/proc`/`/sys` on Linux, synthetic fallback on macOS
- [ ] GPS fields are absent when gpsd unavailable or --no-gps set
- [ ] ghostcam.conf is parsed from /boot/ghostcam.conf if present
- [ ] Server address resolution follows precedence order
- [ ] `CLAUDE.md` updated with camera module structure
