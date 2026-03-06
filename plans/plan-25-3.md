# Implementation Plan: Issue #25 (Bridge-to-Camera Command Channel) & Issue #3 (Camera Management API)

## Overview

These two issues are tightly coupled. Issue #25 establishes a bidirectional command channel from server to camera over the existing QUIC control stream. Issue #3 adds HTTP API endpoints that use that command channel. Work decomposes into five phases executed in order.

## Current State

- **Server** (`server/src/quic.rs`): `let (_, mut control_recv) = connection.accept_bi().await?;` — discards the `SendStream`. Server has NO way to send data back to the camera.
- **Camera** (`camera/src/main.rs`): `let (connection, mut control_send, _control_recv) = quic::connect(...)` — discards the `RecvStream`. Camera never reads from the control stream after connecting.
- **Framing**: `send_hello`/`recv_hello` in `ghostcam/src/quic.rs` use `[4 bytes: u32 BE length][JSON payload]`.

---

## Phase 1: Define Command Protocol in Shared Library (`ghostcam/`)

### Step 1.1: Create `ghostcam/src/command.rs`

```rust
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum CameraCommand {
    // Stream control
    StartVideo,
    StopVideo,
    StartAudio,
    StopAudio,
    StartTelemetry,
    StopTelemetry,

    // Hot configuration (sparse update, only non-None fields changed)
    Configure {
        width: Option<u32>,
        height: Option<u32>,
        fps: Option<u32>,
        bitrate: Option<u32>,
        keyframe_interval: Option<u32>,
    },

    ForceKeyframe,

    // Group reassignment (for issue #3)
    ReassignGroup { group_id: String },

    // Extensible: PTZ, drone, GPIO, etc.
    Custom {
        name: String,
        params: HashMap<String, serde_json::Value>,
    },
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum CommandResponse {
    Ack { command: String },
    Error { command: String, message: String },
}
```

Design decisions:
- `serde(tag = "type")` matches existing `DataChannelMessage` pattern.
- `Configure` uses `Option` fields following `SparseTelemetry` pattern.
- `Custom` with `HashMap<String, Value>` provides PTZ/drone/GPIO extensibility without coupling to hardware.
- `CommandResponse` is optional — fire-and-forget for now, response support added when needed.

### Step 1.2: Add send/recv helpers to `ghostcam/src/quic.rs`

Follow the exact same length-prefixed JSON pattern as `send_hello`/`recv_hello`:

```rust
pub async fn send_command(stream: &mut quinn::SendStream, cmd: &CameraCommand) -> Result<()>
pub async fn recv_command(recv: &mut quinn::RecvStream) -> Result<CameraCommand>
pub async fn send_response(stream: &mut quinn::SendStream, resp: &CommandResponse) -> Result<()>
pub async fn recv_response(recv: &mut quinn::RecvStream) -> Result<CommandResponse>
```

Max message size: 1MB (same guard as frame protocol).

### Step 1.3: Register module in `ghostcam/src/lib.rs`

Add `pub mod command;`

### Step 1.4: Unit tests

- Roundtrip serialization for each `CameraCommand` variant
- Roundtrip for `CommandResponse` variants
- `Custom` with nested arbitrary params
- Unknown `type` produces clear deserialization error

---

## Phase 2: Server-Side Command Sending Infrastructure

### Step 2.1: Add per-camera command channel to `GroupRouter` in `ghostcam/src/router.rs`

```rust
pub struct GroupRouter {
    // ... existing fields ...
    pub command_txs: HashMap<DeviceId, mpsc::Sender<CameraCommand>>,
}
```

Add helper:
```rust
pub fn get_command_tx(&self, device_id: &str) -> Option<mpsc::Sender<CameraCommand>>
```

### Step 2.2: Update `register_camera` signature

```rust
pub fn register_camera(
    &mut self,
    device_id: DeviceId,
    group_id: GroupId,
    capabilities: Vec<String>,
    command_tx: mpsc::Sender<CameraCommand>,  // NEW
)
```

Store in `self.command_txs`. Remove in `unregister_camera`.

### Step 2.3: Update server QUIC handler in `server/src/quic.rs`

Change:
```rust
let (_, mut control_recv) = connection.accept_bi().await?;
```
To:
```rust
let (mut control_send, mut control_recv) = connection.accept_bi().await?;
```

After hello, create command channel and register:
```rust
let (cmd_tx, mut cmd_rx) = mpsc::channel::<CameraCommand>(64);
router.register_camera(hello.device_id.clone(), ..., cmd_tx);
```

Spawn command writer task:
```rust
tokio::spawn(async move {
    while let Some(cmd) = cmd_rx.recv().await {
        if let Err(e) = ghostcam::quic::send_command(&mut control_send, &cmd).await {
            warn!(device_id = %id, error = %e, "failed to send command");
            break;
        }
    }
});
```

### Step 2.4: Add `reassign_camera` to `GroupRouter`

```rust
pub fn reassign_camera(&mut self, device_id: &str, new_group_id: GroupId) -> Result<GroupId, String>
```

Moves camera between groups: removes from old group set, adds to new, updates `CameraState.group_id`. Returns old group_id. Cleans up empty source group.

---

## Phase 3: Camera-Side Command Receiver

### Step 3.1: Retain `RecvStream` in `camera/src/main.rs`

Change:
```rust
let (connection, mut control_send, _control_recv) = quic::connect(...).await?;
```
To:
```rust
let (connection, mut control_send, control_recv) = quic::connect(...).await?;
```

Create `watch` channels for stream control flags:
```rust
let (video_enabled_tx, video_enabled_rx) = watch::channel(true);
let (audio_enabled_tx, audio_enabled_rx) = watch::channel(!args.no_audio);
let (telemetry_enabled_tx, telemetry_enabled_rx) = watch::channel(!args.no_telemetry);
```

Spawn command reader task:
```rust
tokio::spawn(handle_commands(control_recv, device_id, video_enabled_tx, audio_enabled_tx, telemetry_enabled_tx));
```

### Step 3.2: Implement `handle_commands` function

```rust
async fn handle_commands(mut recv: quinn::RecvStream, ...) {
    loop {
        match ghostcam::quic::recv_command(&mut recv).await {
            Ok(cmd) => match cmd {
                CameraCommand::StartVideo => video_tx.send(true),
                CameraCommand::StopVideo => video_tx.send(false),
                CameraCommand::StartAudio => audio_tx.send(true),
                // ... etc
                CameraCommand::Configure { .. } => warn!("not yet implemented"),
                CameraCommand::Custom { name, .. } => warn!("unknown command {name}, ignoring"),
            },
            Err(e) => { info!("command stream ended: {e}"); break; }
        }
    }
}
```

Unknown commands are warned and ignored (forward compatibility).

### Step 3.3: Gate frame sending on enable flags

In the send loop, check `watch` receivers before sending:
```rust
CaptureMessage::VideoNal { .. } => {
    if !*video_enabled_rx.borrow() { continue; }
    // ... existing send logic
}
```

`watch::channel` is ideal: non-blocking `borrow()`, single producer (command reader), single consumer (send loop).

---

## Phase 4: Camera Management API (Issue #3)

### Step 4.1: `GET /api/v1/cameras/{device_id}/status`

Response:
```rust
struct CameraStatus {
    device_id: String,
    group_id: String,
    capabilities: Vec<String>,
    connected_at: u64,
    connection_duration_secs: u64,
    telemetry: Option<TelemetryStatusInfo>,  // latest cpu, temp, memory, uptime
}
```

Reads from `router.cameras` and `router.telemetry` under read lock. **No blockers — standalone endpoint.**

### Step 4.2: `PUT /api/v1/cameras/{device_id}/group`

Request: `{ "group_id": "new-group" }`

Handler pattern (critical for lock safety):
```rust
// 1. Acquire write lock, mutate, clone Sender, release lock
let (old_group, cmd_tx) = {
    let mut router = state.router.write().await;
    let old = router.reassign_camera(&device_id, new_group_id)?;
    let tx = router.command_txs.get(&device_id).cloned();
    (old, tx)
};
// Lock released here

// 2. Send command outside the lock
if let Some(tx) = cmd_tx {
    let _ = tx.send(CameraCommand::ReassignGroup { group_id }).await;
}

// 3. TODO: trigger renegotiation for affected sessions (needs #1)
```

**Never hold `RwLock` across an await point.**

### Step 4.3: `POST /api/v1/cameras/{device_id}/command`

Generic endpoint — accepts any `CameraCommand` as JSON body, sends to camera:
```rust
async fn send_camera_command(
    State(state): State<Arc<AppState>>,
    Path(device_id): Path<String>,
    Json(cmd): Json<CameraCommand>,
) -> Result<StatusCode, (StatusCode, String)>
```

Returns `202 Accepted` (fire-and-forget). Useful for stream control, force keyframe, custom commands.

### Step 4.4: Register routes in `server/src/api.rs`

```rust
.route("/api/v1/cameras/:device_id/status", get(camera_status))
.route("/api/v1/cameras/:device_id/group", put(reassign_camera_group))
.route("/api/v1/cameras/:device_id/command", post(send_camera_command))
```

---

## Phase 5: Documentation and Tests

### Step 5.1: Unit tests in `ghostcam/src/command.rs`
- Serialization roundtrips for all variants

### Step 5.2: Unit tests in `ghostcam/src/router.rs`
- `reassign_camera` moves between groups
- `reassign_camera` for nonexistent camera returns error
- Empty source group cleaned up

### Step 5.3: Update `CLAUDE.md` and `README.md`
- Wire protocol: add command channel subsection
- API Quick Reference: add new endpoints
- Architecture data flow: add bridge-to-camera arrow
- Library structure: add `command.rs`

---

## Dependency Graph

```
Phase 1 (ghostcam lib)
  1.1: command.rs types           ── no deps
  1.2: quic.rs send/recv helpers  ── depends on 1.1
  1.3: lib.rs module              ── depends on 1.1
  1.4: unit tests                 ── depends on 1.1

Phase 2 (server)                  ── depends on Phase 1
  2.1: router.rs command_txs      ── depends on 1.1
  2.2: router.rs register_camera  ── depends on 2.1
  2.3: server/quic.rs handler     ── depends on 1.2, 2.2
  2.4: router.rs reassign_camera  ── depends on 2.1

Phase 3 (camera)                  ── depends on Phase 1
  3.1: camera/main.rs receiver    ── depends on 1.2
  3.2: frame gating               ── depends on 3.1

Phase 4 (API)                     ── depends on Phase 2
  4.1: GET status                 ── depends on 2.1
  4.2: PUT group                  ── depends on 2.4
  4.3: POST command               ── depends on 2.1
  4.4: route registration         ── depends on 4.1-4.3
```

**Phases 2 and 3 are independent and can be developed in parallel.**

---

## Potential Challenges

1. **Lock contention**: `command_txs` is inside `RwLock<GroupRouter>`. Clone the `Sender` under read lock, then `send().await` outside. Pattern shown in Step 4.2.
2. **Command channel backpressure**: `mpsc::channel(64)` provides bounded backpressure. API can use `try_send` for non-blocking.
3. **Control stream lifetime**: When camera disconnects, `cmd_rx.recv()` returns `None` (router unregistration drops `cmd_tx`), writer task terminates cleanly.
4. **Hot reconfiguration**: `Configure` stubbed initially. Full implementation requires restarting `rpicam-vid` subprocess — separate follow-up.
5. **Renegotiation gap**: `reassign_camera_group` updates routing immediately (affects new sessions), but existing sessions won't dynamically gain/lose tracks until #1 is done.
