# Ghostcam

Real-time camera surveillance system. Cameras stream H.264 video and Opus audio over QUIC to a server, which stores recordings locally, relays live feeds via WebRTC, and exposes a REST + SSE API consumed by a Svelte 5 browser viewer.

## Repository Layout

```
ghostcam/
├── ghostcam/     Shared library — wire protocol, types, telemetry, PKI primitives
├── camera/       Camera agent — QUIC/mTLS, capture, recording, enrollment, telemetry
├── server/       Server binary — QUIC ingest, WebRTC egress, HTTP API, Redis telemetry, PostgreSQL
└── ui/           Svelte 5 SPA — live WebRTC view, HLS playback, timeline scrubber, GPS map
```

Each component has its own README:
- [`ghostcam/README.md`](ghostcam/README.md)
- [`camera/README.md`](camera/README.md)
- [`server/README.md`](server/README.md)
- [`ui/README.md`](ui/README.md)

## Architecture

The server is a protocol translator, not an SFU. It does not transcode or mix media. Its job is to forward encoded frames from a camera's ingest slot to any number of subscribed viewer egress handles.

```
┌─────────────┐
│  Camera     │  QUIC/mTLS    ┌──────────────────────────────────────────────┐
│  (Pi, etc.) │──────────────>│  IngestSlot                                  │
└─────────────┘  H.264+Opus   │  ┌────────────────────────────────────────┐  │
                 + telemetry  │  │ QUIC read loops (video, audio, upload) │  │
┌─────────────┐               │  │ broadcast::Sender<VideoFrame>          │  │
│  Camera     │──────────────>│  │ broadcast::Sender<AudioFrame>          │  │
└─────────────┘               │  │ HLS ring buffer                        │  │
                              │  └───────────────┬────────────────────────┘  │
                              │                  │ fan-out                   │
                              │       ┌──────────┼──────────┐                │
                              │       ▼          ▼          ▼                │
                              │   EgressHandle  Handle    Handle             │
                              │   (str0m Rtc)                                │
                              └───────┼──────────┼──────────┼────────────────┘
                                      │ RTP/UDP  │          │
                                      ▼          ▼          ▼
                                  Viewer A   Viewer B   Viewer C
                                  WebRTC     WebRTC     WebRTC
```

### Ingest

Each connected camera has one `IngestSlot`. The slot runs a QUIC read loop independently of viewer count — when no viewers are watching, broadcast sends are no-ops and frames are dropped cheaply. The slot maintains an HLS ring buffer of fMP4 segments for playback.

Cameras send three stream types: persistent `Video` and `Audio` streams (length-prefixed frames), and one-shot upload streams for fMP4 segments, manifests, and telemetry buffers. A persistent `Alerts` stream carries camera→server messages and server→camera commands.

### Egress

Each viewer×camera pair is one `EgressHandle` with its own UDP socket and str0m `Rtc` instance. The handle subscribes to the ingest slot's broadcast channels and drives a WebRTC send loop. When a camera disconnects, its slot closes and all egress handles for that camera receive the closed signal. Other cameras' sessions for the same viewer are unaffected.

The server is ICE-lite — it only responds to STUN binding requests, never initiates. One host candidate is advertised per session using the configured `GHOSTCAM_PUBLIC_IP`.

### Fan-out

- **Ingest is O(cameras):** One QUIC read loop per camera regardless of viewer count.
- **Egress is O(cameras × viewers)** at the send layer only: each handle receives a `Bytes` clone and writes it to its WebRTC track.
- **Zero cost for unwatched cameras:** broadcast sends return immediately with no receivers.

### State and Recovery

The server holds no durable session state. If it restarts, cameras reconnect and re-register their slots; viewers re-request their feeds via SSE + WebRTC. Persistent state (camera enrollment, API tokens, operator accounts) lives in PostgreSQL. Telemetry history lives in Redis.

| State | Storage | On restart |
|-------|---------|------------|
| Camera enrollment, tokens | PostgreSQL | Persists |
| Telemetry history | Redis Streams | Persists |
| Active QUIC connections | Memory | Cameras reconnect |
| WebRTC sessions | Memory | Viewers re-request |
| HLS ring buffer (in-flight) | Memory | Lost; cameras resume recording |

## Quick Start

### Prerequisites

- Rust toolchain ([rustup](https://rustup.rs/))
- [Bun](https://bun.sh/))
- PostgreSQL 16+
- Redis (optional — for telemetry history)
- FFmpeg (for test video generation)

### Build

```bash
cargo build --release
cd ui && bun install && cd ..
```

### Generate test video

```bash
mkdir -p test-data
ffmpeg -f lavfi -i testsrc2=duration=10:size=640x480:rate=30 \
  -c:v libx264 -profile:v baseline -x264-params keyint=60:min-keyint=60 \
  -f h264 test-data/test.h264
```

### Start the server

```bash
# Find your LAN IP:  ipconfig getifaddr en0  (macOS)  |  hostname -I | awk '{print $1}'  (Linux)

GHOSTCAM_DATA_DIR=/tmp/ghostcam-server \
GHOSTCAM_DATABASE_URL=postgres://ghostcam:dev-password@localhost:5432/ghostcam \
GHOSTCAM_REDIS_URL=redis://127.0.0.1:6379 \
GHOSTCAM_PUBLIC_IP=<your-lan-ip> \
./target/release/server
```

> **`GHOSTCAM_PUBLIC_IP` must be a LAN IP, not `127.0.0.1`.** Firefox obfuscates its ICE candidates as mDNS hostnames and cannot route UDP from its LAN-bound socket to loopback. Chrome is unaffected (it also generates a `127.0.0.1` candidate and uses it).

On first start the server runs migrations, generates a CA keypair, and prints an initial password for the `admin` account.

### Start test cameras

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

### Start the viewer

```bash
cd ui && bun run dev
# Open http://localhost:5173 — log in with admin / <printed password>
```

## Ports

| Port | Protocol | Purpose |
|------|----------|---------|
| 4433 | UDP | QUIC — camera ingest |
| 3000 | TCP | HTTP API + static assets |
| 5173 | TCP | Vite dev server (proxies `/api`, `/hls`, `/events` → :3000) |
| _random_ | UDP | WebRTC media — one port per viewer session |

## Configuration

Server is configured via environment variables — see [`server/README.md`](server/README.md).

Camera CLI flags — see [`camera/README.md`](camera/README.md).

## Docker

```bash
docker compose build
docker compose up        # postgres + redis + server + 2 test cameras
```

```bash
# Cross-compile for ARM64 (Raspberry Pi)
docker buildx build --platform linux/arm64 --target camera -t ghostcam-camera .
```

## CI

`.github/workflows/ci.yml` — triggers on push/PR to `main`:

| Job | Checks |
|-----|--------|
| `rust` | `cargo fmt`, `cargo clippy -D warnings`, `cargo test --workspace` |
| `ui` | `bun run check`, `bun run build` |
| `docker` | Builds server and camera images |

## Wire Protocol Summary

### Camera → Server (QUIC)

Persistent streams (stay open for connection lifetime):
- `Alerts` (tag `0x10`) — framed JSON `Alert` messages (handshake, recording events, acks)
- `Video` (tag `0x11`) — length-prefixed H.264 NAL units
- `Audio` (tag `0x12`) — length-prefixed Opus frames

Upload streams (one-shot, camera opens → sends → closes):
- `Segment` (tag `0x00`) — fMP4 media segment
- `Init` (tag `0x01`) — fMP4 init segment
- `Manifest` (tag `0x02`) — HLS playlist
- `TelemetryBuffer` (tag `0x03`) — buffered telemetry array

Server → Camera commands are sent as framed JSON `CameraCommand` messages on the Alerts stream.

See [`ghostcam/src/wire/README.md`](ghostcam/src/wire/README.md) for full protocol details.

### Server → Browser (WebRTC + REST + SSE)

- **Live video**: H.264 RTP, 90 kHz clock, FU-A fragmentation for NALs > 1188 bytes
- **Live audio**: Opus RTP, 48 kHz clock, one packet per frame
- **Camera events**: Server-Sent Events (`/events`) — `camera_online`, `camera_offline`, `telemetry`
- **HLS playback**: fMP4 segments served at `/hls/:device_id/`
- **Telemetry history**: REST at `/api/v1/telemetry/:device_id`

## Key Dependencies

| Dependency | Purpose |
|------------|---------|
| `quinn` 0.11 | QUIC transport |
| `str0m` 0.6 | Sans-I/O WebRTC (ICE-lite) |
| `axum` 0.7 | HTTP framework |
| `rustls` 0.23 | TLS |
| `rcgen` 0.13 | Certificate generation |
| `sqlx` 0.8 | PostgreSQL async |
| `redis` 0.27 | Redis Streams for telemetry |
| `argon2` 0.5 | Password hashing |
| `rmp-serde` 1 | MessagePack for telemetry wire format |
| `tokio` 1 | Async runtime |
| `svelte` 5 | Frontend (runes reactivity) |
| `tailwindcss` 4 | CSS (OKLCH color system) |
| `hls.js` 1 | HLS playback in browser |
| `leaflet` 1.9 | Map integration |

## License

Private / All rights reserved.
