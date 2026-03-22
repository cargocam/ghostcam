# CLAUDE.md — Ghostcam Development Guide

## Documentation Policy

When making changes to the codebase, **always update the relevant READMEs and CLAUDE.md** to reflect those changes. This includes wire protocol messages, API endpoints, CLI flags, architecture, data flow, viewer features, dependencies, and build instructions. Each crate and major subsystem has its own README — keep them in sync with the code.

## What is this project?

Ghostcam is a camera surveillance system. Cameras stream H.264 video + Opus audio over QUIC (mTLS) to a server, which stores recordings locally as fMP4 segments, relays live feeds via WebRTC, and exposes a REST + SSE API consumed by a Svelte 5 browser viewer.

## Repository Layout

```
ghostcam/
├── ghostcam/        Shared library: wire protocol types, config constants, telemetry, PKI primitives
├── camera/          Camera agent: QUIC/mTLS, enrollment, capture, recording, telemetry
├── server/          Server binary: QUIC ingest, WebRTC egress, HTTP API, Redis telemetry, SQLite
├── ui/              Svelte 5 SPA: live WebRTC view, HLS playback, timeline scrubber, GPS map
├── Dockerfile       Multi-stage: server + camera targets
├── docker-compose.yml
└── .github/workflows/ci.yml
```

Each component has a README with full details:
- `ghostcam/README.md` + `ghostcam/src/wire/README.md`
- `camera/README.md`
- `server/README.md` + `server/src/{api,ingest,egress,pki,redis}/README.md`
- `ui/README.md`

## Build & Run

```bash
# One-time: generate test video (requires ffmpeg)
mkdir -p test-data
ffmpeg -f lavfi -i testsrc2=duration=10:size=640x480:rate=30 \
  -c:v libx264 -profile:v baseline -x264-params keyint=60:min-keyint=60 \
  -f h264 test-data/test.h264

# Build all Rust crates
cargo build --release

# Run all tests
cargo test --workspace
```

### Local dev (3 terminals)

```bash
# Terminal 1 — server
# GHOSTCAM_PUBLIC_IP must be your LAN IP, not 127.0.0.1.
# Firefox binds its ICE UDP sockets on the LAN interface and cannot route to loopback.
# Chrome works either way (it also generates a 127.0.0.1 candidate).
# macOS: ipconfig getifaddr en0  |  Linux: hostname -I | awk '{print $1}'
GHOSTCAM_DATA_DIR=/tmp/ghostcam-server \
GHOSTCAM_REDIS_URL=redis://127.0.0.1:6379 \
GHOSTCAM_PUBLIC_IP=<your-lan-ip> \
./target/release/server

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
# Open http://localhost:5173  (login: admin / printed at server first start)
```

## Docker

```bash
docker compose build
docker compose up    # server + 2 test cameras
```

## CI

`.github/workflows/ci.yml` — triggers on push/PR to main:
- **rust**: `cargo fmt`, `cargo clippy -D warnings`, `cargo test --workspace`
- **ui**: `bun install --frozen-lockfile`, `bun run check`, `bun run build`
- **docker**: builds both server and camera targets with BuildKit cache

## Key Ports

- `4433/udp` — QUIC (camera → server)
- `3000/tcp` — HTTP API + static viewer
- `5173/tcp` — Vite dev server (proxies `/api`, `/hls`, `/events` → :3000)

## Logging

```bash
RUST_LOG=server=debug,str0m=warn ./target/release/server
RUST_LOG=camera=debug ./target/release/camera --test-source ...
```

## Architecture

The server is a protocol translator, not an SFU. It forwards encoded frames from camera ingest slots to viewer egress handles without transcoding or mixing.

```
Camera (QUIC/mTLS) ──► IngestSlot ──► broadcast::Sender<VideoFrame>
                         (per camera)          │
                                               ├──► EgressHandle → Viewer A (WebRTC)
                                               ├──► EgressHandle → Viewer B (WebRTC)
                                               └──► EgressHandle → Viewer C (WebRTC)
```

### Ingest

Each camera opens persistent QUIC streams: `Alerts` (bidirectional JSON framing), `Video` (length-prefixed H.264 NALs), `Audio` (length-prefixed Opus frames). One-shot upload streams carry fMP4 segments, manifests, and telemetry buffers.

The `IngestSlot` runs its read loop independently of viewer count. When no viewers are watching, broadcast sends are no-ops.

### Egress

Each viewer×camera pair is one `EgressHandle` — its own UDP socket and str0m `Rtc`. The server is ICE-lite (responds to STUN, never initiates). One host candidate is advertised using `GHOSTCAM_PUBLIC_IP`.

Browser SDP offers may contain mDNS ICE candidates (Firefox). `webrtc.ts` strips all `a=candidate` lines before posting the offer — safe because the server never uses browser candidates.

### Viewer Transport

Camera presence is delivered via **Server-Sent Events** (`/events`). Each camera's live media arrives via a separate **WebRTC** session (`POST /api/v1/watch`). Historical telemetry is fetched on demand via REST (`/api/v1/telemetry/:id`). HLS playback is served at `/hls/:device_id/`.

## ghostcam Library Structure

```
config.rs     Port/size constants (QUIC_PORT=4433, HTTP_PORT=3000, BROADCAST_CAPACITY=2048, ...)
types.rs      DeviceId, UserId, SessionId, TokenId, CertFingerprint, Seq newtypes
telemetry.rs  TelemetryDatagram — sparse MessagePack payload (cpu, temp, mem, gps, ...)
pki.rs        generate_key_pair(), create_self_signed_ca() — rcgen 0.13 wrappers
wire/
  framing.rs  write_frame/read_frame/write_json/read_json — length-prefixed async I/O
  frames.rs   InboundStreamTag enum (Segment/Init/Manifest/TelemetryBuffer/Alerts/Video/Audio)
              VideoFrame, AudioFrame — broadcast channel types
  command.rs  CameraCommand — server→camera tagged JSON (StartVideo, StopVideo, UploadSegment, Reboot, ...)
  alert.rs    Alert — camera→server tagged JSON (Handshake, RecordingSegment, Ack, Enrollment, ...)
```

## Camera Structure

```
main.rs          CLI, reconnect loop with exponential backoff
config.rs        Config resolution: CLI → ghostcam.conf → /etc/ghostcam/server.addr → default
session.rs       Active QUIC session: alerts stream, command gate atomics
enrollment.rs    JWT enrollment handshake
tofu.rs          Server fingerprint pinning (first connect)
quic.rs          QUIC endpoint with mTLS device cert
commands.rs      CameraCommand handler → updates watch channels
capture/         Test sources: video_test.rs (loop H.264), audio_test/ (synthetic Opus)
stream/          Frame senders: video.rs, audio.rs (write to persistent QUIC streams)
recording/       fMP4 ring buffer: muxer, segment, ring_buffer, manifest, uploads
telemetry/       sensors.rs (/proc, /sys, gpsd), buffer.rs (batch upload)
```

## Server Structure

```
main.rs       Env-var config, PKI bootstrap, task spawning, Axum bind
db_trait.rs   Database trait + record types (CameraRecord, SessionRecord, ApiTokenRecord, ...)
db.rs         SqliteDatabase — sqlx SQLite implementation
auth.rs       Token hashing, HMAC, session validation
audit.rs      AuditLogger — HMAC-SHA256 signed audit trail
sse.rs        SseEventBus — per-user broadcast for Server-Sent Events
api/          Axum routes (see server/src/api/README.md)
ingest/       QUIC accept loop, IngestSlot, RoutingRegistry (see server/src/ingest/README.md)
egress/       EgressHandle, SessionManager, RTP packetizer (see server/src/egress/README.md)
pki/          CA bootstrap, device enrollment, revocation (see server/src/pki/README.md)
redis/        Telemetry write/query via Redis Streams (see server/src/redis/README.md)
```

## Viewer Structure

```
auth.ts                Login/logout/session
sse.ts                 SSE client — camera_online/offline events drive WebRTC session lifecycle
connection-manager.ts  SSE event → WebRTC session orchestration
signaling.ts           watchCamera/unwatchCamera (WebRTC SDP), fetchTelemetryRangeCached
webrtc.ts              RTCPeerConnection per camera; stripCandidates() strips mDNS for Firefox
playback.ts            hls.js wrapper for HLS player
telemetry-history.ts   Time-range fetch + cache; nearestTelemetryEntryWithin
stores/
  transport.svelte.ts  SSE connection, WebRTC session map, auth state
  cameras.svelte.ts    Camera registry, live streams, telemetry, online status
  scrubber.svelte.ts   Timeline mode (live/playback), playhead time, mode callbacks
  settings.svelte.ts   Theme, layout, mute state (localStorage)
  groups.svelte.ts     Group list + active group
  alerts.svelte.ts     Disconnect/reconnect notifications
  cameraConfig.svelte.ts  Display name overrides (localStorage)
  videoStats.svelte.ts    Per-track WebRTC inbound-rtp stats
  thumbnails.svelte.ts    Canvas frame thumbnails
```

## Wire Protocol

### QUIC Framing (all control messages)

```
[4 bytes: payload length (u32 BE)] [payload bytes (JSON)]
```

### Camera Stream Tags (first byte of each unidirectional QUIC stream)

| Tag | Value | Type | Description |
|-----|-------|------|-------------|
| `Alerts` | `0x10` | Persistent | Camera→server `Alert` messages |
| `Video` | `0x11` | Persistent | Length-prefixed H.264 NAL units |
| `Audio` | `0x12` | Persistent | Length-prefixed Opus frames |
| `Segment` | `0x00` | Upload | fMP4 media segment |
| `Init` | `0x01` | Upload | fMP4 init segment |
| `Manifest` | `0x02` | Upload | HLS playlist |
| `TelemetryBuffer` | `0x03` | Upload | Batched telemetry array |

### CameraCommand (server → camera, on Alerts stream)

Tagged JSON (`"type"` field): `StartVideo`, `StopVideo`, `StartAudio`, `StopAudio`, `UploadSegment { segment_id }`, `UploadInit`, `Reboot`, `NetworkConfig { ssid, psk }`, `RemoveNetwork { ssid }`, `ListNetworks`, `UpdateAvailable { version, url, checksum }`, `CertRefresh { cert_pem }`, `Unregister`

All carry `seq: Seq` for correlation with `Alert::Ack`.

### Alert (camera → server, on Alerts stream)

Tagged JSON: `Handshake { device_id, cert_fingerprint, capabilities }`, `CapabilityUpdate`, `RecordingSegment { segment_id, duration_ms, size_bytes, start_ts }`, `SegmentEvicted { segment_id }`, `SegmentUploaded { segment_id }`, `SegmentUploadFailed { segment_id, reason }`, `Ack { seq }`, `Enrollment { enrollment_token }`

### RTP (server → browser)

- **Video**: H.264, 90 kHz clock. NAL ≤ 1188 bytes → Single NAL packet. NAL > 1188 bytes → FU-A fragments.
- **Audio**: Opus, 48 kHz clock. One RTP packet per frame.
- Timestamp formula: `(timestamp_us * clock_hz + 500_000) / 1_000_000`

### Telemetry (camera → server upload stream)

MessagePack-encoded `TelemetryDatagram`. All fields `Option<f32>` — only changed values sent per interval. Full heartbeat every 30s. Diff thresholds: CPU 5%, temp 1°C, mem 5 MB, GPS 0.0001°.

## API Quick Reference

Auth: `Authorization: Bearer <token>` or `session=<id>` cookie.

```
POST   /api/v1/auth/login                  { username, password } → session cookie
POST   /api/v1/auth/logout
PATCH  /api/v1/auth/password

GET    /api/v1/cameras                     List enrolled cameras
POST   /api/v1/cameras                     Enroll new camera
GET    /api/v1/cameras/:id                 Camera + latest telemetry
PATCH  /api/v1/cameras/:id                 Update name/group
DELETE /api/v1/cameras/:id                 Revoke

POST   /api/v1/watch                       { device_id, sdp_offer } → { session_id, sdp_answer }
DELETE /api/v1/session/:id                 Tear down WebRTC session
POST   /api/v1/session/:id/ice             Trickle ICE candidate

GET    /api/v1/telemetry/:id/latest        Most recent telemetry
GET    /api/v1/telemetry/:id               ?from=<ms>&to=<ms>&limit=<n>

GET    /hls/:id/playlist.m3u8
GET    /hls/:id/init.mp4
GET    /hls/:id/:segment_id

GET    /events                             SSE stream

GET    /api/v1/tokens                      List API tokens
POST   /api/v1/tokens                      Create token
DELETE /api/v1/tokens/:id                  Revoke token

GET    /healthz                            Always 200 (no auth)
GET    /readyz                             200 when ready (no auth)
```

## Code Conventions

### Rust

- **Error handling**: `anyhow::Result` everywhere (both binary and library crates in this project).
- **Async**: All I/O is tokio async. Blocking work in `tokio::task::spawn_blocking`.
- **Shared state**: `Arc<AppState>`. Keep lock scopes minimal — never hold a lock across an `.await`.
- **Broadcast channels**: `tokio::sync::broadcast` for video/audio fan-out. Lagging receivers drop frames — this is intentional.
- **Logging**: `tracing` crate. Structured fields: `info!(device_id = %id, "connected")`.
- **Dependencies**: All shared deps in workspace `[workspace.dependencies]`.

### Svelte / TypeScript

- **Svelte 5 runes only**: `$state`, `$derived`, `$effect`, `$props()`. No legacy `$:`.
- **Stores**: Exported object literals with `$state` fields — not class-based.
- **Styling**: Tailwind CSS 4 utility classes. OKLCH tokens in `app.css`. `cn()` for merging.
- **Components**: bits-ui primitives in `components/ui/`. Domain components alongside views.
- **localStorage**: Keys prefixed with `ghostcam-`.

## Debugging Tips

- **Firefox WebRTC fails**: Ensure `GHOSTCAM_PUBLIC_IP` is the LAN IP (not `127.0.0.1`). Firefox binds ICE UDP on the LAN interface and cannot route to loopback. `webrtc.ts` already strips mDNS candidates from the SDP offer.
- **No video**: Check server logs for "handshake received". Enable debug: `RUST_LOG=server=debug,str0m=warn`.
- **QUIC refused**: Verify port 4433/udp is open and the server started successfully.
- **Telemetry API 503**: `GHOSTCAM_REDIS_URL` is unset or empty — Redis is required for telemetry history.
- **Camera offline after server restart**: Cameras auto-reconnect with backoff (1s → 30s). Wait or restart cameras manually.
- **str0m API**: Pinned at 0.6.x. Key methods: `Rtc::builder().set_ice_lite(true)`, `sdp_api().accept_offer(offer)`, `rtc.writer(mid)`, `channel.write(binary, data)`.

## Key Dependencies

| Crate/Package | Version | Notes |
|---------------|---------|-------|
| `quinn` | 0.11 | QUIC transport |
| `str0m` | 0.6 | Sans-I/O WebRTC, ICE-lite |
| `axum` | 0.7 | HTTP framework |
| `rustls` | 0.23 | TLS for QUIC |
| `rcgen` | 0.13 | Cert generation. `KeyPair::generate()`, `CertificateParams::self_signed(&kp)` |
| `sqlx` | 0.8 | SQLite async |
| `redis` | 0.27 | Redis Streams for telemetry |
| `argon2` | 0.5 | Password hashing |
| `rmp-serde` | 1 | MessagePack for telemetry wire format |
| `tokio` | 1 | Async runtime |
| `svelte` | 5 | Frontend. Runes: `$state`, `$derived`, `$effect` |
| `tailwindcss` | 4 | OKLCH color system, `@import "tailwindcss"` |
| `hls.js` | 1 | HLS playback in browser |
| `bits-ui` | 2 | Headless component primitives |
| `leaflet` | 1.9 | Map |
