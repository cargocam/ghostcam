# CLAUDE.md — Ghostcam Development Guide

## What is this project?

Ghostcam is a camera surveillance system. Cameras stream H.264 video + Opus audio over QUIC to a bridge server, which translates to WebRTC for browser-based viewing. Cameras are organized into groups; viewers subscribe to a group and receive all feeds over a single PeerConnection.

## Repository Layout

```
ghostcam/
├── ghostcam-common/     Shared library: wire format, handshake, group IDs, config
├── ghostcam-agent/      Test camera QUIC client (loops H.264 file + Opus silence)
├── ghostcam-bridge/     Bridge server: QUIC→WebRTC translator, HTTP API
├── viewer/              Svelte 5 SPA: WebRTC viewer with Tailwind CSS 4
├── test-data/           Generated test H.264 file
├── scripts/             Helper scripts (launch-cameras.sh)
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
cargo build

# Run tests
cargo test

# Run bridge (terminal 1)
cargo run -p ghostcam-bridge -- --public-ip 127.0.0.1

# Run test camera (terminal 2)
cargo run -p ghostcam-agent -- --bridge-addr 127.0.0.1:4433 --device-id cam-01 --group-id default

# Launch N cameras at once
./scripts/launch-cameras.sh 4 default

# Viewer dev server (terminal 3)
cd viewer && bun install && bun run dev

# Build viewer for production
cd viewer && bun run build

# Type-check viewer
cd viewer && bun run check
```

## Key Ports

- `4433/udp` — QUIC (camera → bridge)
- `3000/tcp` — HTTP API + static viewer
- `5173/tcp` — Vite dev server (proxies /api → :3000)

## Logging

```bash
RUST_LOG=ghostcam_bridge=debug,str0m=warn cargo run -p ghostcam-bridge -- --public-ip 127.0.0.1
RUST_LOG=ghostcam_agent=debug cargo run -p ghostcam-agent
```

Default filter: `ghostcam_bridge=info,str0m=warn` (bridge), `ghostcam_agent=info` (agent).

## Architecture

### Data Flow

```
Camera Agent                    Bridge                          Browser
    │                              │                               │
    │── QUIC connect ─────────────>│                               │
    │── DeviceHello (JSON) ───────>│ register in GroupRouter       │
    │── H.264 NAL (uni stream) ──>│ cache SPS/PPS                 │
    │── Opus frame (uni stream) ─>│ broadcast::send()             │
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
```

### Bridge Internal Structure

```
main.rs           CLI parsing, AppState creation, task spawning
quic.rs           QUIC listener → per-camera handler → frame reader
router.rs         GroupRouter: camera registry, broadcast channel, SPS/PPS cache
rtp.rs            H.264 NAL→RTP packetizer (Single NAL + FU-A), timestamp math
webrtc.rs         str0m WebRTC engine: session creation, UDP event loop, frame dispatch
api.rs            Axum HTTP routes + Bearer auth middleware
data_channel.rs   JSON message types for WebRTC data channel
```

### Viewer Internal Structure

```
signaling.ts          HTTP client for SDP exchange + REST API
webrtc.ts             RTCPeerConnection lifecycle, ontrack→store wiring
data-channel.ts       Routes data channel JSON to appropriate stores
stores/
  transport.svelte.ts Orchestrates signaling + WebRTC connection
  cameras.svelte.ts   Camera list, streams, telemetry state
  groups.svelte.ts    Group list and active group
  settings.svelte.ts  Theme, grid layout, view mode (localStorage)
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

### 13-Byte Frame Header

```
Offset  Size  Field
0       1     stream_type (0=video, 1=audio)
1       8     timestamp_us (u64 big-endian, microseconds)
9       4     payload_len (u32 big-endian)
13      var   payload (H.264 NAL or Opus frame)
```

### H.264 RTP Packetization

- NAL ≤ 1188 bytes → Single NAL Unit Packet (raw NAL as RTP payload)
- NAL > 1188 bytes → FU-A fragments (2-byte header: FU indicator + FU header)
- RTP timestamp: `(timestamp_us * 90000 + 500000) / 1_000_000` (integer math, 90kHz clock)
- Marker bit set on last packet of access unit

### Opus RTP

- One RTP packet per Opus frame (no fragmentation)
- RTP timestamp: `(timestamp_us * 48000 + 500000) / 1_000_000` (48kHz clock)
- Test agent sends 3-byte silence frames (`[0xF8, 0xFF, 0xFE]`) every 20ms

### SPS/PPS Caching

The bridge caches the most recent SPS (NAL type 7) and PPS (NAL type 8) per camera. This allows late-joining viewers to receive decoder initialization parameters before the next IDR frame.

## API Quick Reference

All endpoints require `Authorization: Bearer <api-key>` except health checks.

```
POST   /api/v1/watch/{group_id}           SDP offer → session_id + SDP answer
DELETE /api/v1/session/{id}                Tear down WebRTC session
POST   /api/v1/session/{id}/ice           Trickle ICE candidate
GET    /api/v1/groups                      List groups with camera counts
GET    /api/v1/groups/{group_id}/cameras   List cameras in group
GET    /healthz                            Health check (no auth)
GET    /readyz                             Readiness probe (no auth)
```

Default API key: `dev-key` (configurable via `--api-key` or `GHOSTCAM_API_KEY` env).

## Testing

### Unit Tests

```bash
cargo test                    # All Rust tests
cargo test -p ghostcam-common # Frame encode/decode, group hierarchy, NAL parsing
cargo test -p ghostcam-bridge # RTP packetization, timestamp conversion
```

### Manual Integration Test

1. Start bridge: `cargo run -p ghostcam-bridge -- --public-ip 127.0.0.1`
2. Start camera: `cargo run -p ghostcam-agent -- --device-id cam-01 --group-id default`
3. Verify bridge logs: "camera connected", "received device hello", frame receipt
4. Test API: `curl -H "Authorization: Bearer dev-key" http://localhost:3000/api/v1/groups`
5. Start viewer: `cd viewer && bun run dev`, open http://localhost:5173
6. Verify: camera appears in grid, video plays, telemetry updates every 2s

### Multi-Camera Test

```bash
./scripts/launch-cameras.sh 4 default     # 4 cameras in "default" group
./scripts/launch-cameras.sh 2 perimeter   # 2 cameras in "perimeter" group
```

## Debugging Tips

- **No video in browser**: Check bridge logs for "WebRTC session created". Ensure SPS/PPS NALs are being cached (bridge logs NAL types at debug level). Try `RUST_LOG=ghostcam_bridge=debug`.
- **QUIC connection refused**: Verify bridge QUIC listener started on 4433/udp. Check firewall rules. Agent uses self-signed certs with server verification disabled (dev only).
- **ICE failures**: Bridge uses ICE-lite with a single host candidate. Ensure `--public-ip` matches the IP reachable from the browser. No STUN/TURN needed for localhost.
- **Broadcast lag**: If viewers fall behind, frames are dropped (broadcast channel). Increase channel capacity in `router.rs` if needed (default: 4096).
- **str0m issues**: Pin str0m at 0.6.x. API changed significantly between versions. See `webrtc.rs` for correct method signatures.

## Key Dependencies

| Crate/Package | Version | Notes |
|---------------|---------|-------|
| str0m | 0.6 | Sans-I/O WebRTC, ICE-lite. `SdpApi::add_media(kind, dir, stream_id, track_id)`, `Writer::write(pt, Instant, MediaTime, data)` |
| quinn | 0.11 | QUIC transport. `Endpoint::server()`, uni/bi streams |
| rustls | 0.23 | TLS backend for quinn. Features: `ring`, `std` (no default) |
| rcgen | 0.13 | Self-signed certs. `KeyPair::generate()`, `CertificateParams::self_signed(&key_pair)` |
| axum | 0.7 | HTTP. `from_fn` middleware for auth |
| svelte | 5 | Runes: `$state`, `$derived`, `$effect` |
| tailwindcss | 4 | OKLCH color system, `@import "tailwindcss"` |
| bits-ui | 2 | Headless component primitives |
