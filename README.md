# Ghostcam

Real-time camera surveillance system. Cameras stream H.264 video and Opus audio over QUIC to a server, which stores recorded segments, relays live feeds via WebRTC, and exposes a REST + SSE API consumed by a Svelte 5 browser viewer.

## Architecture

```
┌─────────────┐  QUIC/mTLS  ┌──────────────────────────────────┐  WebRTC   ┌──────────────┐
│  Camera 1   │────────────>│                                  │──────────>│              │
│  (camera)   │  H.264+Opus │          server-solo             │  RTP      │   Browser    │
└─────────────┘             │                                  │  video    │   Viewer     │
                            │  ┌────────────┐  ┌────────────┐ │  + audio  │              │
┌─────────────┐  QUIC/mTLS  │  │ server-    │  │   egress   │ │           │  SSE events  │
│  Camera 2   │────────────>│  │   core     │  │  (str0m)   │ │<──────────│  WebRTC SDP  │
│  (camera)   │             │  │            │  │            │ │           └──────────────┘
└─────────────┘             │  │  ingest    │  │  HTTP API  │ │
                            │  │  Redis     │  │  HLS       │ │
┌─────────────┐  QUIC/mTLS  │  │  PKI/auth  │  │  SSE       │ │
│  Camera N   │────────────>│  └────────────┘  └────────────┘ │
│  (camera)   │             │                                  │
└─────────────┘             │  SQLite (owner/cameras/sessions) │
                            └──────────────────────────────────┘
                                     Redis (telemetry)
```

### Crates

| Crate | Role |
|-------|------|
| `ghostcam` | Shared library — wire types, config constants, telemetry structs |
| `camera` | Camera agent — QUIC/mTLS client, H.264+Opus streaming, HLS recording, enrollment, telemetry |
| `server-core` | Shared server library — ingest pipeline, WebRTC egress, HTTP API, Redis telemetry, SSE, PKI |
| `server-solo` | Single-node binary wrapping server-core — SQLite DB, Argon2id auth, env-var config |
| `server-multi` | Multi-node binary (future distributed deployment) |

### UI (`ui/`)

Svelte 5 SPA — WebRTC live view, HLS playback with timeline scrubber, SSE-driven camera list, telemetry charts, GPS map, authentication.

## Quick Start

### Prerequisites

- Rust toolchain ([rustup](https://rustup.rs/))
- [Bun](https://bun.sh/)
- Redis (for telemetry storage — optional for basic video)
- FFmpeg (for generating test video)

### 1. Build

```bash
cargo build --release
cd ui && bun install && cd ..
```

### 2. Generate test video

```bash
mkdir -p test-data
ffmpeg -f lavfi -i testsrc2=duration=10:size=640x480:rate=30 \
  -c:v libx264 -profile:v baseline -x264-params keyint=60:min-keyint=60 \
  -f h264 test-data/test.h264
```

### 3. Start the server

```bash
# Find your LAN IP first — required for Firefox WebRTC compatibility
# macOS: ipconfig getifaddr en0
# Linux: hostname -I | awk '{print $1}'

GHOSTCAM_DATA_DIR=/tmp/ghostcam-server \
GHOSTCAM_REDIS_URL=redis://127.0.0.1:6379 \
GHOSTCAM_PUBLIC_IP=<your-lan-ip> \
./target/release/server-solo
```

> **Important:** `GHOSTCAM_PUBLIC_IP` must be your LAN IP, not `127.0.0.1`. Firefox obfuscates its ICE candidates as mDNS hostnames and cannot reach the loopback interface from a non-loopback socket. Chrome generates a `127.0.0.1` candidate and works either way.

On first start, server-solo creates the data directory, generates a CA keypair, and prints an initial password for the `admin` account.

### 4. Start test cameras

```bash
for i in 1 2 3; do
  mkdir -p /tmp/ghostcam-cam0$i/segments
  ./target/release/camera \
    --test-source \
    --server-addr 127.0.0.1:4433 \
    --data-dir /tmp/ghostcam-cam0$i \
    --segment-dir /tmp/ghostcam-cam0$i/segments \
    --no-tofu &
done
```

### 5. Start the viewer

```bash
cd ui && bun run dev
```

Open http://localhost:5173, log in with `admin` / (password printed at server start).

## Configuration

### Server (`server-solo`)

All configuration via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `GHOSTCAM_DATA_DIR` | `/var/ghostcam` | Data directory (CA keys, DB, certs) |
| `GHOSTCAM_PUBLIC_IP` | `127.0.0.1` | Public IP for WebRTC ICE host candidates |
| `GHOSTCAM_HTTP_PORT` | `3000` | HTTP API + static assets port |
| `GHOSTCAM_QUIC_PORT` | `4433` | QUIC ingest port for cameras |
| `GHOSTCAM_REDIS_URL` | _(none)_ | Redis URL for telemetry storage. Telemetry API disabled if unset. |

### Camera (`camera`)

| Flag | Default | Description |
|------|---------|-------------|
| `--server-addr` | _(from enrollment / ghostcam.conf)_ | Server QUIC address `host:port` |
| `--test-source` | off | Use test H.264 file + synthetic audio instead of real capture |
| `--test-video` | `test-data/test.h264` | H.264 file for `--test-source` |
| `--data-dir` | `/var/ghostcam` | Device cert, config, enrollment state |
| `--segment-dir` | `/var/ghostcam/segments` | fMP4 ring buffer for HLS recording |
| `--no-audio` | off | Disable audio capture |
| `--no-gps` | off | Disable GPS via gpsd |
| `--enrollment-jwt` | _(none)_ | Skip QR-based enrollment with a pre-issued JWT |
| `--no-tofu` | off | Disable TOFU server fingerprint verification (dev/testing) |

## Ports

| Port | Protocol | Service |
|------|----------|---------|
| 4433 | UDP | QUIC — camera ingest |
| 3000 | TCP | HTTP API + static viewer (production) |
| 5173 | TCP | Vite dev server (proxies `/api`, `/hls`, `/events` → :3000) |
| _random_ | UDP | WebRTC media egress (one port per viewer session) |

## HTTP API

Authentication: `Authorization: Bearer <token>` header or `session=<id>` cookie. Tokens are created via `POST /api/v1/tokens`. Login issues a session cookie.

### Auth

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/auth/login` | `{ username, password }` → session cookie |
| `POST` | `/api/v1/auth/logout` | Clear session cookie |
| `PATCH` | `/api/v1/auth/password` | Change password |

### Cameras

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/cameras` | List all enrolled cameras |
| `POST` | `/api/v1/cameras` | Enroll a new camera (issues cert signed by CA) |
| `GET` | `/api/v1/cameras/:id` | Camera details, connection status, latest telemetry |
| `PATCH` | `/api/v1/cameras/:id` | Update camera (display name, group) |
| `DELETE` | `/api/v1/cameras/:id` | Revoke camera enrollment |

### WebRTC Sessions

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/watch` | `{ device_id, sdp_offer }` → `{ session_id, sdp_answer }` |
| `DELETE` | `/api/v1/session/:id` | Tear down session |
| `POST` | `/api/v1/session/:id/ice` | Trickle ICE candidate |

### Telemetry

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/telemetry/:id/latest` | Most recent telemetry point |
| `GET` | `/api/v1/telemetry/:id?from=&to=&limit=` | Time-range query (Unix ms) |

### HLS

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/hls/:id/playlist.m3u8` | Live HLS manifest (camera's ring buffer) |
| `GET` | `/hls/:id/init.mp4` | fMP4 init segment |
| `GET` | `/hls/:id/:segment_id` | fMP4 media segment |

### API Tokens

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/tokens` | List API tokens |
| `POST` | `/api/v1/tokens` | Create token |
| `DELETE` | `/api/v1/tokens/:id` | Revoke token |

### Health

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/healthz` | Always `200 ok` (no auth) |
| `GET` | `/readyz` | `200` when DB and ingest listener are ready (no auth) |

### SSE

`GET /events` — Server-sent event stream. Events: `camera_online`, `camera_offline`, `telemetry`, `enrollment_pending`.

## Wire Protocol

### QUIC (Camera → Server)

**Control stream** (bidirectional, opened by camera):
```
Camera → Server: [4 bytes: JSON length (u32 BE)] [JSON: DeviceHello]
Server → Camera: [4 bytes: JSON length (u32 BE)] [JSON: CameraCommand] (repeated)
```

**Frame streams** (unidirectional, one per frame):
```
┌──────────────┬───────────────────┬──────────────────┬─────────┐
│ stream_type  │   timestamp_us    │   payload_len    │ payload │
│   (1 byte)   │   (8 bytes BE)    │   (4 bytes BE)   │  (var)  │
└──────────────┴───────────────────┴──────────────────┴─────────┘
```
`stream_type`: `0` = H.264 NAL, `1` = Opus frame, `2` = MessagePack SparseTelemetry

### WebRTC (Server → Browser)

- **Video**: H.264 RTP (RFC 6184), 90kHz clock, FU-A fragmentation for NALs > 1188 bytes
- **Audio**: Opus RTP (RFC 7587), 48kHz clock, one packet per frame

## Viewer Features

- Password-protected login
- Multi-camera grid — auto-fit and 1+5 featured layout
- Live WebRTC video + audio with per-camera mute
- HLS playback mode with timeline scrubber for recorded footage
- Telemetry history — scrub through historical CPU, memory, temperature, GPS
- GPS map with camera markers and playback trail overlay
- Camera online/offline status, display name overrides
- Dark/light/system theme
- Mobile responsive

## Docker

```bash
docker compose build
docker compose up       # server-solo + 2 test cameras
```

```bash
# Cross-compile for ARM64 (Raspberry Pi)
docker buildx build --platform linux/arm64 --target agent -t ghostcam-camera .
```

## CI

`.github/workflows/ci.yml` — triggers on push/PR to `main`:

| Job | Checks |
|-----|--------|
| `rust` | `cargo fmt`, `cargo clippy -D warnings`, `cargo test` |
| `ui` | `bun run check`, `bun run build` |
| `docker` | Builds `bridge` and `agent` images |

## Project Structure

```
ghostcam/
├── ghostcam/src/          Shared library (wire types, telemetry, config)
│   ├── pki.rs             CA and certificate management
│   ├── types.rs           DeviceId, UserId, shared newtypes
│   ├── telemetry.rs       SparseTelemetry, GpsData, diff/merge
│   └── wire/              Framed wire protocol types
├── camera/src/            Camera agent
│   ├── session.rs         QUIC handshake + streaming loop
│   ├── enrollment.rs      TOFU + JWT enrollment
│   ├── recording/         HLS fMP4 ring buffer
│   ├── stream/            Per-stream senders (video/audio/telemetry)
│   ├── telemetry/         Platform telemetry readers (/proc, /sys, gpsd)
│   ├── capture/           Test sources (video_test.rs, audio_test/)
│   └── config.rs          Config resolution (CLI → conf file → enrollment)
├── server-core/src/       Shared server library
│   ├── ingest/            QUIC listener, per-camera handler, frame relay
│   ├── egress/            str0m WebRTC engine, RTP packetizer, HLS
│   ├── api/               Axum routes (cameras, watch, telemetry, HLS, SSE, auth)
│   ├── redis/             Telemetry write/query (Redis Streams)
│   ├── pki/               CA, enrollment signing
│   └── db.rs              SQLite schema (cameras, sessions, tokens)
├── server-solo/src/       Single-node binary
│   └── main.rs            Env-var config, SQLite setup, server-core wiring
├── server-multi/src/      Multi-node binary (placeholder)
├── ui/src/
│   ├── App.svelte         Root — auth gate, scrubber wiring, view routing
│   └── lib/
│       ├── auth.ts        Login/logout/session
│       ├── sse.ts         SSE event stream client
│       ├── webrtc.ts      RTCPeerConnection, stripCandidates, ICE rewrite
│       ├── signaling.ts   watchCamera/unwatchCamera, telemetry fetch
│       ├── telemetry-history.ts  Range fetch + caching
│       ├── playback.ts    HLS.js wrapper
│       ├── stores/        Svelte 5 $state stores
│       └── components/    UI components (CameraCard, MapView, HlsPlayer, ...)
├── specs/                 Protocol and API specifications
├── plans/                 Implementation plans (plan-01 through plan-12)
├── Dockerfile             Multi-stage: server-solo + camera targets
├── docker-compose.yml     server-solo + 2 test cameras
└── fly.toml               Fly.io deployment config
```

## Key Dependencies

| Dependency | Version | Purpose |
|------------|---------|---------|
| str0m | 0.6 | Sans-I/O WebRTC (ICE-lite) |
| quinn | 0.11 | QUIC transport |
| axum | 0.7 | HTTP framework |
| rustls | 0.23 | TLS for QUIC |
| rcgen | 0.13 | Certificate generation |
| sqlx | 0.8 | SQLite async |
| redis | 0.27 | Redis Streams for telemetry |
| argon2 | 0.5 | Password hashing |
| rmp-serde | 1 | MessagePack (telemetry wire) |
| tokio | 1 | Async runtime |
| svelte | 5 | Frontend (runes reactivity) |
| tailwindcss | 4 | CSS (OKLCH color system) |
| hls.js | 1 | HLS playback in browser |

## License

Private / All rights reserved.
