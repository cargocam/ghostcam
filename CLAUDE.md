# CLAUDE.md — Ghostcam Development Guide

## Documentation Policy

When making changes to the codebase, **always update README.md and CLAUDE.md** to reflect those changes. This includes new/changed wire protocol messages, API endpoints, CLI flags, architecture, data flow, viewer features, dependencies, and build instructions. These files are the source of truth — keep them in sync with the code.

## What is this project?

Ghostcam is a camera surveillance system. Cameras stream H.264 video + Opus audio over QUIC to a bridge server, which translates to WebRTC for browser-based viewing. Cameras are organized into groups; viewers subscribe to a group and receive all feeds over a single PeerConnection.

## Repository Layout

```
ghostcam/
├── ghostcam/            Shared library: wire format, handshake, group IDs, config, router, RTP, data channel, H.264 parser, QUIC helpers, stream I/O
├── camera/              Camera agent: QUIC client, real capture (rpicam-vid, cpal, telemetry) or --test-source mode; launch-cameras.sh helper
├── server/              Bridge server: CLI, QUIC listener, str0m WebRTC engine, Axum HTTP API
├── ui/                  Svelte 5 SPA: WebRTC viewer with Tailwind CSS 4
├── test-data/           Generated test H.264 file
├── Dockerfile           Multi-stage: bridge + agent targets (cargo-chef cached)
├── .dockerignore
├── .github/workflows/   CI pipeline (rust, ui, docker jobs)
└── docker-compose.yml   Bridge + 2 camera containers
```

## Build & Run

```bash
# One-time: generate test video (requires ffmpeg)
mkdir -p test-data
ffmpeg -f lavfi -i testsrc2=duration=10:size=640x480:rate=30 \
  -c:v libx264 -profile:v baseline -x264-params keyint=60:min-keyint=60 \
  -f h264 test-data/test.h264

# Build all Rust crates
cargo build --release

# Run tests
cargo test
```

### Local dev (3 terminals)

```bash
# Terminal 1 — server
# GHOSTCAM_PUBLIC_IP must be your LAN IP (not 127.0.0.1) so Firefox can connect via WebRTC.
# Find it with: ipconfig getifaddr en0   (macOS) or  hostname -I | awk '{print $1}'  (Linux)
GHOSTCAM_DATA_DIR=/tmp/ghostcam-server \
GHOSTCAM_REDIS_URL=redis://127.0.0.1:6379 \
GHOSTCAM_PUBLIC_IP=<your-lan-ip> \
./target/release/server-solo

# Terminal 2 — test cameras (3 in parallel)
for i in 1 2 3; do
  mkdir -p /tmp/ghostcam-cam0$i/segments
  ./target/release/camera \
    --test-source --server-addr 127.0.0.1:4433 \
    --data-dir /tmp/ghostcam-cam0$i \
    --segment-dir /tmp/ghostcam-cam0$i/segments \
    --no-tofu &
done

# Terminal 3 — viewer dev server
cd ui && bun install && bun run dev
# Open http://localhost:5173
```

```bash
# Build viewer for production
cd ui && bun run build

# Type-check viewer
cd ui && bun run check
```

## Docker

```bash
docker compose build     # Build bridge + agent images
docker compose up        # Run bridge + 2 test cameras
```

Multi-stage Dockerfile: `chef` → `planner` → `builder` (Rust), `ui-builder` (Bun/Svelte), `test-data` (ffmpeg), then final `bridge` and `agent` targets. Uses `cargo-chef` for dependency layer caching. Both camera commands in docker-compose.yml include `--test-source` since containers don't have rpicam-vid.

## CI

`.github/workflows/ci.yml` — triggers on push/PR to main:
- **rust**: fmt check, clippy (`-D warnings`), test (all crates). Installs libasound2-dev, libopus-dev.
- **ui**: `bun install --frozen-lockfile`, `bun run check`, `bun run build`.
- **docker** (needs: rust, ui): builds both `bridge` and `agent` targets with BuildKit cache.

## Key Ports

- `4433/udp` — QUIC (camera → bridge)
- `3000/tcp` — HTTP API + static viewer
- `5173/tcp` — Vite dev server (proxies /api → :3000)

## Logging

```bash
RUST_LOG=server-solo=debug,str0m=warn ./target/release/server-solo
RUST_LOG=camera=debug ./target/release/camera --test-source ...
```

Default filter: `info` for all crates, `str0m=warn` suppressed.

## Architecture

### Data Flow

```
Camera                          Server                          Browser
    │                              │                               │
    │── QUIC connect ─────────────>│                               │
    │── DeviceHello (JSON) ───────>│ register in GroupRouter       │
    │<── CameraCommand (JSON) ────│ stream control / config       │
    │── H.264 NAL (uni stream) ──>│ cache SPS/PPS                 │
    │── Opus frame (uni stream) ─>│ broadcast::send()             │
    │── Telemetry (uni stream) ──>│ decode + merge + broadcast    │
    │                              │                               │
    │                              │<── POST /watch (SDP offer) ───│
    │                              │    create str0m Rtc            │
    │                              │    add video+audio tracks      │
    │                              │    add data channel            │
    │                              │──> SDP answer ────────────────>│
    │                              │                               │
    │                              │    broadcast::recv() ──>      │
    │                              │    packetize H.264 (FU-A)     │
    │                              │    write RTP via str0m ──────>│ video.srcObject
    │                              │    write Opus RTP ───────────>│ audio
    │                              │    data channel telemetry ───>│ camera list, metrics
    │                              │                               │
    │── New camera QUIC connect ─>│ event_tx(CameraJoined)        │
    │                              │    add_media(video+audio)      │
    │                              │    sdp_api.apply() → offer     │
    │                              │    DC: camera_join ───────────>│
    │                              │    DC: renegotiate ───────────>│ setRemoteDescription
    │                              │                               │ createAnswer
    │                              │<── DC: sdp_answer ────────────│
    │                              │    accept_answer(pending)      │
    │                              │    DC: track_map ────────────>│ ontrack → stream
```

### Library Structure (`ghostcam/`)

```
audit.rs          AuditEvent/AuditEntry/AuditLogger: HMAC-SHA256 signed audit trail, broadcast channel
command.rs        CameraCommand/CommandResponse enums (bridge→camera control), serde tagged JSON
config.rs         Port/MTU constants
data_channel.rs   DataChannelMessage types (WebRTC data channel JSON)
frame.rs          13-byte wire format encode/decode
group.rs          Hierarchical GroupId
h264.rs           Annex-B NAL parser (parse_h264_file) + streaming NalParser
hello.rs          DeviceHello handshake
quic.rs           Shared QUIC/TLS: cert generation, server/client config, hello/command send/recv
router.rs         GroupRouter: camera registry, broadcast channels (frames + events), SPS/PPS cache, telemetry state, command channels, reassign
rtp.rs            H.264 NAL→RTP packetizer (Single NAL + FU-A), timestamp math
stream.rs         send_video_frame, send_audio_frame, send_telemetry_frame, OPUS_SILENCE
telemetry.rs      TelemetryData, SparseTelemetry, GpsData, diff/merge, MessagePack encode/decode
```

### Camera Internal Structure

```
main.rs                CLI parsing, capture orchestration, QUIC reconnect loop, command handler
quic.rs                Quinn QUIC client (connect)
capture/mod.rs         CaptureMessage enum (VideoNal, Audio, Telemetry)
capture/video.rs       rpicam-vid subprocess + streaming NalParser
capture/audio.rs       cpal default input → mono → resample → Opus encode
capture/telemetry.rs   /proc, /sys readers (Linux) + gpsd TCP client (optional)
```

### Server Internal Structure

```
main.rs           CLI parsing, AppState creation, task spawning
quic.rs           QUIC listener → per-camera handler → frame reader, audit + metrics instrumentation
webrtc.rs         str0m WebRTC engine: session creation, UDP event loop, frame dispatch
api.rs            Axum HTTP routes + Bearer auth middleware, audit events, /metrics endpoint
metrics.rs        Prometheus metrics (prometheus-client): gauges, counters, text encoding
```

### Viewer Internal Structure

```
signaling.ts          HTTP client for SDP exchange + REST API
webrtc.ts             RTCPeerConnection lifecycle, ontrack→store wiring (video + audio tracks), dynamic renegotiation (SDP offer/answer via data channel)
data-channel.ts       Routes data channel JSON to appropriate stores
stores/
  transport.svelte.ts Orchestrates signaling + WebRTC connection + auto-reconnect, audio wiring
  cameras.svelte.ts   Camera list, video + audio streams, telemetry state
  groups.svelte.ts    Group list and active group
  settings.svelte.ts  Theme, grid layout, view mode, global/per-camera mute (localStorage)
  alerts.svelte.ts    Disconnect/reconnect notifications
  cameraConfig.svelte.ts   Per-camera display name overrides
  videoStats.svelte.ts     Per-track WebRTC stats
  thumbnails.svelte.ts     Canvas-captured frame thumbnails
```

## Code Conventions

### Rust

- **Error handling**: `anyhow::Result` for binary crates, concrete errors in library code.
- **Async**: All I/O is tokio async. Blocking work must be `tokio::task::spawn_blocking`.
- **Shared state**: `Arc<AppState>` with `RwLock<GroupRouter>`. Acquire read lock for queries, write lock for mutations. Keep lock scopes minimal.
- **Frame broadcasting**: `tokio::sync::broadcast` channel (capacity 4096). Receivers must keep up or frames are dropped (lagging).
- **Logging**: `tracing` crate. Use structured fields: `info!(device_id = %id, "message")`.
- **Dependencies**: All shared deps declared in workspace `[workspace.dependencies]`.

### Svelte / TypeScript

- **Svelte 5 runes**: Use `$state`, `$derived`, `$effect`, `$bindable`, `$props()`. No legacy `$:` reactivity.
- **State management**: Exported object-literal stores with `$state` fields and methods. Not class-based.
- **Styling**: Tailwind CSS 4 utility classes. OKLCH color tokens defined in `app.css`. Use `cn()` from `$lib/utils` for class merging.
- **Components**: bits-ui primitives in `components/ui/`, domain components alongside views.
- **localStorage keys**: Prefixed with `ghostcam-` (not `kodama-`).

## Wire Protocol Details

### QUIC Control Stream (Bidirectional)

Camera opens one bidirectional stream for the handshake. Length-prefixed JSON:

```
[4 bytes: JSON length (u32 BE)] [JSON: DeviceHello]
```

```json
{ "device_id": "cam-01", "group_id": "default", "capabilities": ["h264", "opus"] }
```

After DeviceHello, the control stream remains open for bridge→camera commands.

### QUIC Command Channel (Bidirectional, bridge→camera)

After the handshake, the bridge sends `CameraCommand` messages on the same bidirectional control stream using the same length-prefixed JSON framing:

```
[4 bytes: JSON length (u32 BE)] [JSON: CameraCommand]
```

`CameraCommand` is a tagged enum (`"type"` field):

| Type | Fields | Description |
|------|--------|-------------|
| `start_video` | — | Resume video streaming |
| `stop_video` | — | Pause video streaming |
| `start_audio` | — | Resume audio streaming |
| `stop_audio` | — | Pause audio streaming |
| `start_telemetry` | — | Resume telemetry |
| `stop_telemetry` | — | Pause telemetry |
| `configure` | `width?`, `height?`, `fps?`, `bitrate?`, `keyframe_interval?` | Hot-update capture params (sparse, only set fields change) |
| `force_keyframe` | — | Request immediate IDR frame |
| `reassign_group` | `group_id` | Move camera to new group (server handles routing) |
| `custom` | `name`, `params` | Extensible: PTZ, drone, GPIO. Unknown → warn + ignore |

The camera reads commands via a spawned task and uses `tokio::sync::watch` channels to gate frame sending (non-blocking `borrow()`). Unknown commands are logged and ignored for forward compatibility.

### 13-Byte Frame Header (Unidirectional Streams)

```
Offset  Size  Field
0       1     stream_type (0=video, 1=audio, 2=telemetry)
1       8     timestamp_us (u64 big-endian, microseconds)
9       4     payload_len (u32 big-endian)
13      var   payload (H.264 NAL, Opus frame, or MessagePack SparseTelemetry)
```

Max frame size: 1 MB. One unidirectional stream per frame.

### H.264 RTP Packetization

- Non-VCL NALs (SPS, PPS, SEI — types 0,6,7,8) buffered in `nal_accumulator` until a VCL NAL arrives (type 1=slice or 5=IDR), then sent together as Annex-B (`[00 00 00 01][NAL]...`). str0m handles STAP-A/FU-A.
- NAL ≤ 1188 bytes → Single NAL Unit Packet (raw NAL as RTP payload)
- NAL > 1188 bytes → FU-A fragments (2-byte header: FU indicator + FU header)
- RTP timestamp: `(timestamp_us * 90000 + 500000) / 1_000_000` (integer math, 90kHz clock)
- Marker bit set on last packet of access unit

### Opus RTP

- One RTP packet per Opus frame (no fragmentation)
- RTP timestamp: `(timestamp_us * 48000 + 500000) / 1_000_000` (48kHz clock)
- Test agent sends 3-byte silence frames (`[0xF8, 0xFF, 0xFE]`) every 20ms

### Telemetry Wire Format (StreamType::Telemetry = 2)

Camera sends MessagePack-encoded `SparseTelemetry` on unidirectional QUIC streams using the same 13-byte frame header. The `SparseTelemetry` struct uses short serde field names (`c`, `t`, `m`, `u`, `l`, `tx`, `rx`, `g`, `f`) and `Option` wrappers — only changed fields are set.

- Camera polls system metrics every 2s, computes diff against previous state using thresholds (CPU: 5%, temp: 1C, memory: 5MB, network: 10KB, GPS: 0.0001deg)
- Full heartbeat sent every 30s (`full: true`, all fields populated)
- Server decodes SparseTelemetry, merges into `router.telemetry` and `session.telemetry_state`, sends JSON `DataChannelMessage::Telemetry` to viewer

### SPS/PPS Caching

The bridge caches the most recent SPS (NAL type 7) and PPS (NAL type 8) per camera. This allows late-joining viewers to receive decoder initialization parameters before the next IDR frame.

### WebRTC Data Channel Messages

Data channel named "telemetry", JSON text (not binary). Bidirectional.

**Bridge → Viewer:**

| Type | When | Key Fields |
|------|------|------------|
| `cameras` | Session creation | Full camera list with `device_id`, `group_id`, `capabilities` |
| `camera_join` | Camera connects to group | Single camera object |
| `camera_leave` | Camera disconnects | `device_id` |
| `telemetry` | When camera sends telemetry | `cpu_percent`, `temp_celsius`, `memory_mb`, `uptime_secs`, optional `gps` |
| `track_map` | Data channel opens or after renegotiation | Maps SDP `mid` → `device_id` + `kind` ("video"/"audio") |
| `renegotiate` | Camera join/leave triggers track changes | `sdp_offer` — server-initiated SDP offer for dynamic renegotiation |

**Viewer → Bridge:**

| Type | When | Key Fields |
|------|------|------------|
| `sdp_answer` | Response to `renegotiate` | `sdp_answer` — viewer's SDP answer completing renegotiation |

Telemetry is event-driven from camera frames (StreamType::Telemetry = 2, MessagePack SparseTelemetry). The server decodes, merges into per-camera state, and forwards as JSON on the data channel. In test-source mode, no telemetry is sent.

## API Quick Reference

All endpoints require `Authorization: Bearer <api-key>` except health checks.

```
POST   /api/v1/watch/{group_id}           SDP offer → session_id + SDP answer
DELETE /api/v1/session/{id}                Tear down WebRTC session
POST   /api/v1/session/{id}/ice           Trickle ICE candidate
GET    /api/v1/groups                      List groups with camera counts
GET    /api/v1/groups/{group_id}/cameras   List cameras in group
GET    /api/v1/cameras/{device_id}/status  Camera health, connection info, telemetry
PUT    /api/v1/cameras/{device_id}/group   Reassign camera to different group
POST   /api/v1/cameras/{device_id}/command Send CameraCommand to camera (fire-and-forget, 202)
GET    /healthz                            Health check (no auth)
GET    /readyz                             Readiness probe (no auth)
GET    /metrics                            Prometheus metrics (no auth)
```

Default API key: `dev-key` (configurable via `--api-key` or `GHOSTCAM_API_KEY` env).
HMAC key: `dev-hmac-key` (configurable via `--hmac-key` or `GHOSTCAM_HMAC_KEY` env).

## Testing

### Unit Tests

```bash
cargo test                    # All Rust tests
cargo test -p ghostcam        # Frame encode/decode, group hierarchy, NAL parsing, telemetry, NalParser, command roundtrip, router reassign, audit events
cargo test -p server          # RTP packetization, timestamp conversion
```

### Manual Integration Test

1. Start server: `cargo run -p server -- --public-ip 127.0.0.1`
2. Start camera: `cargo run -p camera -- --test-source --device-id cam-01 --group-id default`
3. Verify server logs: "camera connected", "received device hello", frame receipt
4. Test API: `curl -H "Authorization: Bearer dev-key" http://localhost:3000/api/v1/groups`
5. Start viewer: `cd ui && bun run dev`, open http://localhost:5173
6. Verify: camera appears in grid, video plays, telemetry updates every 2s

### Multi-Camera Test

```bash
./camera/launch-cameras.sh 4 default     # 4 cameras in "default" group
./camera/launch-cameras.sh 2 perimeter   # 2 cameras in "perimeter" group
```

## Debugging Tips

- **No video in browser**: Check server logs for "WebRTC session created". Ensure SPS/PPS NALs are being cached (server logs NAL types at debug level). Try `RUST_LOG=server=debug`.
- **QUIC connection refused**: Verify server QUIC listener started on 4433/udp. Check firewall rules. Camera uses self-signed certs with server verification disabled (dev only).
- **ICE failures**: Server uses ICE-lite with a single host candidate. Ensure `--public-ip` matches the IP reachable from the browser. No STUN/TURN needed for localhost.
- **Broadcast lag**: If viewers fall behind, frames are dropped (broadcast channel). Increase channel capacity in `router.rs` if needed (default: 4096).
- **str0m issues**: Pin str0m at 0.6.x. API changed significantly between versions. See `webrtc.rs` for correct method signatures.

## Key Dependencies

| Crate/Package | Version | Notes |
|---------------|---------|-------|
| str0m | 0.6 | Sans-I/O WebRTC, ICE-lite. `SdpApi::add_media(kind, dir, stream_id, track_id)`, `Writer::write(pt, Instant, MediaTime, data)` |
| quinn | 0.11 | QUIC transport. `Endpoint::server()`, uni/bi streams |
| rustls | 0.23 | TLS backend for quinn. Features: `ring`, `std` (no default) |
| rcgen | 0.13 | Self-signed certs. `KeyPair::generate()`, `CertificateParams::self_signed(&key_pair)` |
| axum | 0.7 | HTTP. `from_fn_with_state` middleware for auth + audit |
| ring | 0.17 | HMAC-SHA256 for audit log integrity (transitive via rustls) |
| prometheus-client | 0.23 | Prometheus metrics: gauges, counters, text encoding |
| rmp-serde | 1 | MessagePack ser/de for telemetry wire format |
| cpal | 0.15 | Cross-platform audio input (camera) |
| opus | 0.3 | Opus audio encoding (camera) |
| svelte | 5 | Runes: `$state`, `$derived`, `$effect` |
| tailwindcss | 4 | OKLCH color system, `@import "tailwindcss"` |
| bits-ui | 2 | Headless component primitives |
