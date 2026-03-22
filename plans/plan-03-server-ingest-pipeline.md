# Plan 3: Server Core — Ingest Pipeline

**Status:** Not started
**Branch:** `rewrite`
**Depends on:** Plan 1 (shared types), Plan 2 (database layer)
**Unlocks:** Plan 4 (PKI & enrollment), Plan 5 (Redis integration), Plan 6 (egress & HTTP API)

---

## 1. Goal

Implement the server-side QUIC ingest pipeline: the connection accept loop with mTLS verification, IngestSlot structure and lifecycle, broadcast fan-out for video/audio/telemetry, the routing registry, alert dispatch, command sending, upload stream handling with request coalescing, and live subscriber demand tracking.

After this plan, the server can accept QUIC connections from cameras, read frames from all channels, broadcast them, handle alerts, send commands, and receive upload streams. No WebRTC egress, no HTTP API, no Redis — those are later plans. Testing uses a mock QUIC camera client.

---

## 2. Architecture

### 2.1 Data Flow

```
Camera (QUIC/mTLS)
  ├── Alerts stream (uni, camera→server)     → alert_handler task
  ├── Video stream (uni, camera→server)      → broadcast::Sender<VideoFrame>
  ├── Audio stream (uni, camera→server)      → broadcast::Sender<AudioFrame>
  ├── Telemetry (QUIC datagrams)             → broadcast::Sender<TelemetryDatagram>
  ├── Upload streams (uni, camera→server)    → upload_handler task
  └── Commands stream (uni, server→camera)   ← mpsc::Receiver<Command>
```

### 2.2 Decoupling Model

IngestSlot broadcasts via `tokio::sync::broadcast` channels. It has no knowledge of downstream consumers. EgressHandles (Plan 6) will subscribe to these channels independently. The broadcast channel is the seam — no trait, no `dyn`, just concrete types and channels.

Subscriber demand tracking uses `AtomicUsize` counters on the IngestSlot. Future EgressHandles will hold `Arc` references to these counters and increment/decrement them directly.

---

## 3. Crate Changes

### 3.1 New Dependencies

**Workspace `Cargo.toml`** — add:
```toml
[workspace.dependencies]
quinn = "0.11"
tokio-util = { version = "0.7", features = ["codec"] }
```

**`server-core/Cargo.toml`** — add:
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
thiserror.workspace = true
tracing.workspace = true
serde.workspace = true
serde_json.workspace = true
rmp-serde.workspace = true
async-trait.workspace = true
ring.workspace = true

[dev-dependencies]
tokio = { workspace = true, features = ["full", "test-util"] }
```

---

## 4. Implementation Details

### 4.1 Frame Types (`server-core/src/frames.rs`)

Concrete frame types carried over broadcast channels. These wrap raw bytes with metadata.

```rust
/// A video frame read from the QUIC video stream.
#[derive(Debug, Clone)]
pub struct VideoFrame {
    pub data: Bytes,
}

/// An audio frame read from the QUIC audio stream.
#[derive(Debug, Clone)]
pub struct AudioFrame {
    pub data: Bytes,
}
```

`TelemetryDatagram` from the `ghostcam` crate is used directly for the telemetry broadcast channel.

### 4.2 Upload Stream Type Tag

The spec lists upload stream disambiguation as an open question. We resolve it with a 1-byte type tag as the first byte of every camera-initiated inbound unidirectional stream:

```rust
/// First byte written by the camera on each inbound unidirectional stream.
#[repr(u8)]
pub enum UploadStreamType {
    Segment = 0x00,
    Init = 0x01,
    Manifest = 0x02,
    TelemetryBuffer = 0x03,
}
```

Command-triggered uploads (`upload_segment`, `upload_init`) and camera-initiated uploads (manifest push, telemetry buffer) all use this tag. The server reads one byte, dispatches accordingly.

This is a wire protocol addition — update `specs/wire-protocol.md` and `CLAUDE.md` to document it.

### 4.3 IngestSlot (`server-core/src/ingest/slot.rs`)

```rust
pub struct IngestSlot {
    /// Camera identity
    pub device_id: DeviceId,
    pub user_id: UserId,

    /// Current camera capabilities (which streams are active)
    pub capabilities: Arc<RwLock<Vec<StreamKind>>>,

    /// Broadcast channels — always present, silent when no data
    pub video_tx: broadcast::Sender<VideoFrame>,
    pub audio_tx: broadcast::Sender<AudioFrame>,
    pub telemetry_tx: broadcast::Sender<TelemetryDatagram>,

    /// Latest HLS manifest (pushed by camera)
    pub manifest: Arc<RwLock<Option<String>>>,

    /// Send commands to the camera
    pub commands_tx: mpsc::Sender<Command>,

    /// Live subscriber demand counters
    pub video_subscribers: Arc<AtomicUsize>,
    pub audio_subscribers: Arc<AtomicUsize>,

    /// Monotonically increasing command sequence number
    seq: Arc<AtomicU64>,

    /// In-flight segment uploads for request coalescing
    pub segments: Arc<RwLock<HashMap<String, SegmentState>>>,

    /// Cancellation token for coordinated teardown
    cancel: CancellationToken,
}
```

#### Construction and Lifecycle

```rust
impl IngestSlot {
    /// Create a new slot and spawn all read loop tasks.
    /// Returns the slot (wrapped in Arc) and a JoinHandle for the supervisor task.
    pub async fn spawn(
        device_id: DeviceId,
        user_id: UserId,
        connection: quinn::Connection,
        alerts_stream: quinn::RecvStream,
        video_stream: quinn::RecvStream,
        audio_stream: quinn::RecvStream,
        commands_stream: quinn::SendStream,
    ) -> (Arc<Self>, JoinHandle<()>);

    /// Allocate the next command sequence number.
    pub fn next_seq(&self) -> u64;

    /// Send a command to the camera.
    pub async fn send_command(&self, command: Command) -> Result<()>;

    /// Shut down the slot: cancel all tasks, close the QUIC connection.
    pub fn shutdown(&self);
}
```

The `spawn` method:
1. Creates all broadcast channels (capacity `BROADCAST_CAPACITY`)
2. Creates the `mpsc::channel(64)` for commands
3. Spawns concurrent tasks:
   - `alert_reader` — reads length-prefixed JSON from alerts stream, dispatches
   - `video_reader` — reads length-prefixed frames from video stream, broadcasts
   - `audio_reader` — reads length-prefixed frames from audio stream, broadcasts
   - `telemetry_reader` — reads QUIC datagrams, decodes MessagePack, broadcasts
   - `command_writer` — reads from mpsc receiver, writes length-prefixed JSON to commands stream
   - `upload_acceptor` — accepts inbound uni streams, reads type tag, dispatches
4. Spawns a supervisor task that waits for any task to finish (or the cancel token), then tears down everything

#### Segment State for Request Coalescing

```rust
pub enum SegmentState {
    /// Upload is in progress. Waiters will be notified on completion.
    Uploading {
        waiters: Vec<oneshot::Sender<Result<Bytes>>>,
    },
    /// Upload complete. Data buffered for 60 seconds.
    Buffered {
        data: Bytes,
        expires_at: Instant,
    },
}
```

### 4.4 Alert Handler (`server-core/src/ingest/alerts.rs`)

Dispatches on `Alert::type`:

```rust
async fn handle_alert(slot: &Arc<IngestSlot>, alert: Alert, db: &dyn Database) -> Result<()> {
    match alert {
        Alert::Handshake { .. } => {
            // Ignored post-accept (already handled in accept loop)
            tracing::debug!("ignoring duplicate handshake");
        }
        Alert::CapabilityUpdate { streams } => {
            *slot.capabilities.write().await = streams;
            tracing::info!(device_id = %slot.device_id, "capability update");
        }
        Alert::RecordingSegment { segment_id, start_ts, end_ts, size_bytes, .. } => {
            // Will write to Redis in Plan 5. For now, log.
            tracing::info!(device_id = %slot.device_id, segment_id, "recording segment");
        }
        Alert::SegmentEvicted { segment_id } => {
            // Will tombstone in Redis in Plan 5. For now, log.
            tracing::info!(device_id = %slot.device_id, segment_id, "segment evicted");
        }
        Alert::SegmentUploaded { seq, segment_id } => {
            // Update in-flight segment map to Buffered state
            handle_segment_uploaded(slot, &segment_id).await;
        }
        Alert::SegmentUploadFailed { seq, segment_id, reason } => {
            // Remove from in-flight map, notify waiters with error
            handle_segment_upload_failed(slot, &segment_id, reason).await;
        }
        Alert::Ack { cmd, seq } => {
            // Will be used by enrollment (Plan 4). For now, log.
            tracing::info!(device_id = %slot.device_id, cmd, seq, "ack received");
        }
        // Other alert types logged but not acted on until later plans
        other => {
            tracing::info!(device_id = %slot.device_id, ?other, "alert received");
        }
    }
    Ok(())
}
```

### 4.5 Upload Handler (`server-core/src/ingest/uploads.rs`)

Handles inbound unidirectional streams from the camera.

```rust
async fn handle_upload_stream(
    slot: &Arc<IngestSlot>,
    mut stream: quinn::RecvStream,
) -> Result<()> {
    // Read 1-byte type tag
    let mut tag = [0u8; 1];
    stream.read_exact(&mut tag).await?;

    match UploadStreamType::try_from(tag[0])? {
        UploadStreamType::Segment => handle_segment_upload(slot, stream).await,
        UploadStreamType::Init => handle_init_upload(slot, stream).await,
        UploadStreamType::Manifest => handle_manifest_push(slot, stream).await,
        UploadStreamType::TelemetryBuffer => handle_telemetry_buffer(slot, stream).await,
    }
}

/// Read the full stream into memory and replace the slot's manifest.
async fn handle_manifest_push(slot: &Arc<IngestSlot>, stream: quinn::RecvStream) -> Result<()>;

/// Read the full segment upload, move to Buffered state, notify waiters.
async fn handle_segment_upload(slot: &Arc<IngestSlot>, stream: quinn::RecvStream) -> Result<()>;

/// Read the init segment upload, store in slot.
async fn handle_init_upload(slot: &Arc<IngestSlot>, stream: quinn::RecvStream) -> Result<()>;

/// Read the full telemetry buffer, decode MessagePack array.
/// In Plan 5 this will write to Redis. For now, log entry count.
async fn handle_telemetry_buffer(slot: &Arc<IngestSlot>, stream: quinn::RecvStream) -> Result<()>;
```

### 4.6 Subscriber Demand Tracking (`server-core/src/ingest/demand.rs`)

```rust
/// Called when a client changes mode. Updates subscriber counts and
/// sends start/stop commands to the camera as needed.
pub async fn update_subscriber_demand(
    slot: &Arc<IngestSlot>,
    old_mode: Option<ClientMode>,
    new_mode: ClientMode,
) -> Result<()> {
    // Compute delta for video and audio subscribers
    let was_live = old_mode.map_or(false, |m| m == ClientMode::Live);
    let is_live = new_mode == ClientMode::Live;

    if !was_live && is_live {
        // Joining live
        let prev_video = slot.video_subscribers.fetch_add(1, Ordering::SeqCst);
        let prev_audio = slot.audio_subscribers.fetch_add(1, Ordering::SeqCst);
        if prev_video == 0 {
            slot.send_command(Command::StartVideo { seq: slot.next_seq() }).await?;
        }
        if prev_audio == 0 {
            slot.send_command(Command::StartAudio { seq: slot.next_seq() }).await?;
        }
    } else if was_live && !is_live {
        // Leaving live
        let prev_video = slot.video_subscribers.fetch_sub(1, Ordering::SeqCst);
        let prev_audio = slot.audio_subscribers.fetch_sub(1, Ordering::SeqCst);
        if prev_video == 1 {
            slot.send_command(Command::StopVideo { seq: slot.next_seq() }).await?;
        }
        if prev_audio == 1 {
            slot.send_command(Command::StopAudio { seq: slot.next_seq() }).await?;
        }
    }
    Ok(())
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ClientMode {
    Live,
    Playback,
    Map,
}
```

### 4.7 Routing Registry (`server-core/src/ingest/registry.rs`)

```rust
pub struct RoutingRegistry {
    /// Connected cameras, keyed by owning user then device.
    cameras: RwLock<HashMap<UserId, HashMap<DeviceId, Arc<IngestSlot>>>>,
}

impl RoutingRegistry {
    pub fn new() -> Self;

    /// Register an IngestSlot. If a stale slot exists for the same device_id,
    /// it is shut down and replaced.
    pub async fn register(&self, slot: Arc<IngestSlot>);

    /// Remove an IngestSlot on camera disconnect.
    pub async fn unregister(&self, device_id: &DeviceId);

    /// Look up a slot by device_id. Returns None if camera is not connected.
    pub async fn get_slot(&self, device_id: &DeviceId) -> Option<Arc<IngestSlot>>;

    /// List all connected cameras for a user.
    pub async fn list_slots(&self, user_id: &UserId) -> Vec<Arc<IngestSlot>>;

    /// List all connected device_ids for a user.
    pub async fn list_device_ids(&self, user_id: &UserId) -> Vec<DeviceId>;

    /// Check if a device is currently connected.
    pub async fn is_connected(&self, device_id: &DeviceId) -> bool;
}
```

Lock discipline: read lock for all queries, write lock only for register/unregister. Lock scopes are minimal — clone data under lock, process outside.

### 4.8 QUIC Accept Loop (`server-core/src/ingest/accept.rs`)

```rust
pub async fn run_accept_loop(
    endpoint: quinn::Endpoint,
    registry: Arc<RoutingRegistry>,
    db: Arc<dyn Database>,
    cancel: CancellationToken,
) -> Result<()> {
    loop {
        tokio::select! {
            _ = cancel.cancelled() => break,
            incoming = endpoint.accept() => {
                let Some(incoming) = incoming else { break };
                let registry = registry.clone();
                let db = db.clone();
                tokio::spawn(async move {
                    if let Err(e) = handle_connection(incoming, registry, db).await {
                        tracing::warn!("connection failed: {e}");
                    }
                });
            }
        }
    }
    Ok(())
}

async fn handle_connection(
    incoming: quinn::Incoming,
    registry: Arc<RoutingRegistry>,
    db: Arc<dyn Database>,
) -> Result<()> {
    let connection = incoming.await?;

    // 1. Extract client certificate from the TLS session
    let peer_certs = connection.peer_identity()
        .and_then(|id| id.downcast::<Vec<rustls::pki_types::CertificateDer>>().ok())
        .ok_or_else(|| anyhow::anyhow!("no client certificate"))?;

    // 2. Compute fingerprint of the device cert (first cert in chain)
    let fingerprint = ghostcam::pki::cert_fingerprint(&peer_certs[0])?;

    // 3. Check for user association cert (second cert in chain)
    let has_association_cert = peer_certs.len() >= 2;

    if !has_association_cert {
        // Route to enrollment handler (Plan 4). For now, reject.
        tracing::info!(?fingerprint, "enrollment connection — not yet implemented");
        connection.close(1u32.into(), b"enrollment not implemented");
        return Ok(());
    }

    // 4. Look up camera by fingerprint in the database
    let camera = db.get_camera_by_fingerprint(&fingerprint).await?
        .ok_or_else(|| anyhow::anyhow!("device not enrolled"))?;

    // 5. Update last_seen
    db.update_last_seen(&camera.device_id).await?;

    // 6. Accept the three camera-initiated uni streams (Alerts, Video, Audio)
    let alerts_stream = accept_uni_stream(&connection).await?;
    let video_stream = accept_uni_stream(&connection).await?;
    let audio_stream = accept_uni_stream(&connection).await?;

    // 7. Open the Commands stream (server → camera)
    let commands_stream = connection.open_uni().await?;

    // 8. Read the handshake alert
    let handshake = read_handshake(&mut alerts_stream).await?;
    if handshake.protocol_version != ghostcam::config::PROTOCOL_VERSION {
        connection.close(2u32.into(), b"unsupported protocol version");
        return Err(anyhow::anyhow!("unsupported protocol version"));
    }

    tracing::info!(
        device_id = %camera.device_id,
        fw_version = %handshake.fw_version,
        "camera connected"
    );

    // 9. Create IngestSlot and register
    let (slot, supervisor) = IngestSlot::spawn(
        camera.device_id.clone(),
        camera.user_id.clone(),
        connection,
        alerts_stream,
        video_stream,
        audio_stream,
        commands_stream,
    ).await;

    registry.register(slot.clone()).await;

    // 10. Wait for the slot supervisor to finish (camera disconnect or error)
    supervisor.await?;

    // 11. Unregister on disconnect
    registry.unregister(&camera.device_id).await;
    tracing::info!(device_id = %camera.device_id, "camera disconnected");

    Ok(())
}
```

### 4.9 QUIC Server Configuration (`server-core/src/ingest/quic_config.rs`)

```rust
/// Build a Quinn server endpoint with mTLS.
/// - `server_cert_pem`: PEM server certificate
/// - `server_key_pem`: PEM server private key
/// - `ca_cert_pem`: PEM CA certificate for verifying client certs
/// - `bind_addr`: UDP socket address to bind
pub fn build_server_endpoint(
    server_cert_pem: &str,
    server_key_pem: &str,
    ca_cert_pem: &str,
    bind_addr: SocketAddr,
) -> Result<quinn::Endpoint>;
```

The server TLS config:
- Presents the server certificate
- Requires client certificates
- Verifies client certs against the CA cert (for enrolled cameras) or accepts any client cert (for enrollment — the fingerprint is the identity)
- Uses a custom `ClientCertVerifier` that accepts any valid cert chain but does NOT reject based on CA trust — the application layer handles enrollment vs normal routing based on cert count. The CA trust check for user association certs happens at the application layer after reading the handshake.

**Note on mTLS strategy:** The QUIC/TLS layer accepts any client presenting a certificate (no CA verification at TLS level). This allows both enrollment connections (device cert only, self-signed) and normal connections (device cert + user association cert) to succeed at the TLS layer. Application-layer code then:
- Counts certs to determine enrollment vs normal
- For normal connections: verifies user association cert against CA + checks revocation
- For enrollment connections: routes to enrollment handler (Plan 4)

This is the pragmatic approach given that device certs are self-signed and wouldn't pass CA verification anyway.

### 4.10 Module Structure

```
server-core/src/
├── lib.rs
├── db.rs                        # Database trait (from Plan 2)
├── auth.rs                      # Password/token utilities (from Plan 2)
├── frames.rs                    # VideoFrame, AudioFrame types
└── ingest/
    ├── mod.rs                   # re-exports
    ├── slot.rs                  # IngestSlot struct + spawn + lifecycle
    ├── alerts.rs                # Alert dispatch
    ├── uploads.rs               # Upload stream handling + segment coalescing
    ├── demand.rs                # Subscriber demand tracking
    ├── registry.rs              # RoutingRegistry
    ├── accept.rs                # QUIC accept loop + connection handler
    └── quic_config.rs           # Quinn server endpoint builder
```

---

## 5. Spec Updates

### 5.1 Wire Protocol Addition

Add to `specs/wire-protocol.md` §4 (Stream Framing) or as a new §4.1:

> **Upload stream type tag:** Every camera-initiated inbound unidirectional stream begins with a 1-byte type tag identifying the upload type:
>
> | Tag | Value | Description |
> |-----|-------|-------------|
> | `0x00` | Segment | fMP4 segment upload (follows `upload_segment` command) |
> | `0x01` | Init | fMP4 init segment upload (follows `upload_init` command) |
> | `0x02` | Manifest | HLS manifest push (camera-initiated) |
> | `0x03` | TelemetryBuffer | Buffered telemetry upload (camera-initiated) |
>
> The server reads this byte before dispatching the stream to the appropriate handler. The remaining bytes are the raw upload payload with no further framing.

This resolves the open question in `ingest.md` §10 about concurrent `upload_init` and `upload_segment` disambiguation.

---

## 6. Testing Plan

### 6.1 Test Infrastructure

A reusable mock QUIC camera client for integration tests:

```rust
/// A mock camera that connects to the server over QUIC and
/// can send alerts, frames, telemetry, and upload streams.
struct MockCamera {
    connection: quinn::Connection,
    alerts_tx: quinn::SendStream,     // Alerts stream (camera → server)
    video_tx: quinn::SendStream,      // Video stream (camera → server)
    audio_tx: quinn::SendStream,      // Audio stream (camera → server)
    commands_rx: quinn::RecvStream,    // Commands stream (server → camera)
}

impl MockCamera {
    /// Connect to the server, open the 3 outbound streams,
    /// accept the inbound Commands stream.
    async fn connect(addr: SocketAddr, device_cert: ..., user_cert: ...) -> Result<Self>;

    /// Send a handshake alert.
    async fn send_handshake(&mut self, fw_version: &str, streams: Vec<StreamKind>) -> Result<()>;

    /// Send an arbitrary alert.
    async fn send_alert(&mut self, alert: &Alert) -> Result<()>;

    /// Send a video frame (length-prefixed bytes).
    async fn send_video_frame(&mut self, data: &[u8]) -> Result<()>;

    /// Send an audio frame (length-prefixed bytes).
    async fn send_audio_frame(&mut self, data: &[u8]) -> Result<()>;

    /// Send a telemetry datagram (MessagePack).
    async fn send_telemetry(&self, datagram: &TelemetryDatagram) -> Result<()>;

    /// Open an upload stream with the given type tag and write data.
    async fn send_upload(&self, upload_type: UploadStreamType, data: &[u8]) -> Result<()>;

    /// Read the next command from the Commands stream.
    async fn recv_command(&mut self) -> Result<Command>;
}
```

A helper to set up a complete test environment:

```rust
/// Spin up a server QUIC endpoint with an in-memory SQLite database,
/// pre-enrolled camera, and return everything needed for testing.
struct TestEnv {
    server_addr: SocketAddr,
    registry: Arc<RoutingRegistry>,
    db: Arc<SqliteDatabase>,
    device_cert: ...,
    user_cert: ...,
    cancel: CancellationToken,
}

impl TestEnv {
    async fn setup() -> Result<Self>;
    async fn teardown(self);
}
```

### 6.2 Unit Tests — Frame Types

**Location:** `server-core/src/frames.rs`

| Test | Description |
|------|-------------|
| `video_frame_clone` | VideoFrame can be cloned (required for broadcast) |
| `audio_frame_clone` | AudioFrame can be cloned |
| `upload_stream_type_roundtrip` | All UploadStreamType variants convert to/from u8 |
| `upload_stream_type_invalid` | Invalid u8 value returns error |

### 6.3 Unit Tests — Subscriber Demand

**Location:** `server-core/src/ingest/demand.rs`

| Test | Description |
|------|-------------|
| `first_live_sends_start` | `update_subscriber_demand(None → Live)` increments video+audio to 1, returns StartVideo + StartAudio commands |
| `second_live_no_command` | Two clients go Live; second does not trigger start commands (count goes 1→2) |
| `last_live_leaves_sends_stop` | Only client leaves Live → Playback; count 1→0, triggers StopVideo + StopAudio |
| `not_last_live_no_stop` | Two Live clients, one leaves; count 2→1, no stop command |
| `playback_to_live` | Client switches Playback → Live; triggers start if count was 0 |
| `map_to_live` | Client switches Map → Live; same as above |
| `live_to_map` | Client switches Live → Map; decrements, triggers stop if count was 1 |
| `playback_to_map_no_change` | Neither mode is Live; no count change, no commands |

For these tests, create an IngestSlot with a mock commands channel and verify commands sent.

### 6.4 Unit Tests — Segment Coalescing

**Location:** `server-core/src/ingest/uploads.rs`

| Test | Description |
|------|-------------|
| `request_absent_segment` | Request a segment not in the map → creates `Uploading` entry |
| `request_uploading_segment_adds_waiter` | Request same segment while uploading → second waiter added, no new upload command |
| `upload_complete_notifies_waiters` | Complete upload → all waiters receive the data |
| `upload_complete_moves_to_buffered` | After completion, segment is `Buffered` with 60s TTL |
| `request_buffered_segment` | Request a Buffered segment → served immediately, TTL reset |
| `buffered_segment_expires` | After 60s the entry is evicted |
| `upload_failed_notifies_waiters_with_error` | `SegmentUploadFailed` → waiters receive error |

### 6.5 Unit Tests — Routing Registry

**Location:** `server-core/src/ingest/registry.rs`

| Test | Description |
|------|-------------|
| `register_and_get` | Register a slot, get by device_id → Some |
| `get_unregistered` | Get unknown device_id → None |
| `unregister` | Register then unregister → get returns None |
| `list_slots_for_user` | Register 3 slots for user A, 2 for user B → list(A) returns 3 |
| `list_device_ids` | Register 3 slots → list_device_ids returns 3 IDs |
| `is_connected` | Register → true, unregister → false |
| `replace_stale_slot` | Register slot for device X, register new slot for same device X → old slot is shut down, new slot returned by get |
| `concurrent_register_unregister` | Spawn 10 concurrent register/unregister tasks → no panic, consistent state |

For these tests, create IngestSlots with dummy broadcast channels (no actual QUIC connection needed — just the struct fields).

### 6.6 Unit Tests — Alert Handling

**Location:** `server-core/src/ingest/alerts.rs`

| Test | Description |
|------|-------------|
| `capability_update_updates_slot` | Send CapabilityUpdate alert → slot.capabilities reflects new streams |
| `segment_uploaded_moves_to_buffered` | Set up Uploading entry with a waiter, handle SegmentUploaded → waiter receives data, state is Buffered |
| `segment_upload_failed_errors_waiters` | Set up Uploading with waiter, handle SegmentUploadFailed → waiter receives error |
| `duplicate_handshake_ignored` | Handle a second Handshake alert → no error, logged |
| `unknown_alert_logged` | Handle an alert type that has no handler → no error |

### 6.7 Integration Tests — Full Connection Lifecycle

**Location:** `server-core/tests/ingest_integration.rs`

These tests use `TestEnv` + `MockCamera`. Each test spins up a real QUIC server on localhost with an in-memory SQLite DB.

| Test | Description |
|------|-------------|
| `camera_connects_and_handshakes` | MockCamera connects, sends handshake → server logs connection, slot appears in registry |
| `camera_rejected_without_enrollment` | MockCamera with unknown fingerprint connects → connection closed, no slot in registry |
| `camera_sends_video_frames` | MockCamera sends 10 video frames → subscriber on video_tx receives all 10 |
| `camera_sends_audio_frames` | MockCamera sends 5 audio frames → subscriber on audio_tx receives all 5 |
| `camera_sends_telemetry` | MockCamera sends telemetry datagram → subscriber on telemetry_tx receives it with correct fields |
| `camera_receives_commands` | Server sends StartVideo command via slot.send_command → MockCamera reads it from Commands stream |
| `camera_sends_manifest_push` | MockCamera opens upload stream with tag 0x02, writes manifest → slot.manifest is updated |
| `camera_sends_telemetry_buffer` | MockCamera opens upload stream with tag 0x03, writes MessagePack array → server reads and decodes (logs for now) |
| `camera_disconnect_unregisters` | MockCamera connects, then drops connection → slot removed from registry |
| `stale_slot_replaced` | MockCamera connects, disconnects, reconnects → new slot replaces old in registry |
| `capability_update_reflects` | MockCamera sends CapabilityUpdate → slot.capabilities updated |
| `demand_tracking_start_stop` | Subscribe to video_tx (simulating an EgressHandle going Live) by incrementing video_subscribers from 0→1, call demand update → MockCamera receives StartVideo command |
| `multiple_cameras_same_user` | Two MockCameras with different fingerprints connect for same user → both in registry, independent slots |
| `protocol_version_mismatch` | MockCamera sends handshake with protocol_version=99 → connection closed |

### 6.8 Integration Tests — Segment Upload Coalescing (End-to-End)

| Test | Description |
|------|-------------|
| `segment_upload_e2e` | Server sends UploadSegment command → MockCamera opens upload stream (tag 0x00), writes data, sends SegmentUploaded alert → segment state moves to Buffered, data matches |
| `segment_upload_failed_e2e` | Server sends UploadSegment → MockCamera sends SegmentUploadFailed with reason Evicted → waiters notified with error |
| `coalesced_segment_request` | Two concurrent requests for same segment → only one UploadSegment command sent to camera, both waiters receive data |

### 6.9 Build Validation

| Check | Command | Expected |
|-------|---------|----------|
| Workspace compiles | `cargo build` | All crates compile |
| Unit tests | `cargo test -p server-core` | All unit tests pass |
| Integration tests | `cargo test -p server-core --test ingest_integration` | All integration tests pass |
| Clippy | `cargo clippy -- -D warnings` | Clean |
| Format | `cargo fmt --check` | Clean |

---

## 7. Validation Checklist

After completing this plan, verify:

- [ ] `cargo build` succeeds for all crates
- [ ] `cargo test` passes all tests listed above
- [ ] MockCamera can connect to the server over QUIC with mTLS
- [ ] Server rejects cameras with unknown cert fingerprints
- [ ] Server correctly reads the handshake alert and creates an IngestSlot
- [ ] Video frames broadcast to all subscribers
- [ ] Audio frames broadcast to all subscribers
- [ ] Telemetry datagrams decoded from MessagePack and broadcast
- [ ] Commands are sent from server to camera via the Commands stream
- [ ] Upload streams are dispatched by the 1-byte type tag
- [ ] Manifest push updates the slot's in-memory manifest
- [ ] Segment upload coalescing works: Uploading → Buffered → Expired lifecycle
- [ ] Subscriber demand tracking sends start/stop commands on 0↔1 transitions
- [ ] Routing registry correctly registers, unregisters, and replaces stale slots
- [ ] Camera disconnect triggers slot teardown and unregistration
- [ ] Protocol version mismatch closes the connection
- [ ] All QUIC connections use mTLS (server cert + client cert)
- [ ] `specs/wire-protocol.md` updated with upload stream type tag
- [ ] `CLAUDE.md` updated with new module structure
