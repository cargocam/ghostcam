# server

Bridge server that accepts H.264/Opus streams from cameras over QUIC and translates them to WebRTC for browser-based viewing. Exposes an HTTP API for session management and camera metadata.

## Usage

```bash
cargo run -p server -- --public-ip 127.0.0.1

# With options
cargo run -p server -- \
  --public-ip 192.168.1.10 \
  --quic-port 4433 \
  --http-port 3000 \
  --api-key my-secret \
  --viewer-dir ui/build
```

## CLI Flags

| Flag | Default | Env Var | Description |
|------|---------|---------|-------------|
| `--public-ip` | `127.0.0.1` | | IP advertised in ICE candidates |
| `--quic-port` | `4433` | | QUIC listener port (cameras) |
| `--http-port` | `3000` | | HTTP API + viewer port |
| `--api-key` | `dev-key` | `GHOSTCAM_API_KEY` | Bearer token for API auth |
| `--hmac-key` | `dev-hmac-key` | `GHOSTCAM_HMAC_KEY` | HMAC-SHA256 key for audit log integrity |
| `--viewer-dir` | — | | Serve static viewer files (optional) |

## Architecture

```
Camera (QUIC)          Bridge                          Browser (WebRTC)
    │                     │                                │
    ├─ DeviceHello ──────>│ register in GroupRouter         │
    │<── CameraCommand ──│ stream control / config         │
    ├─ H.264 NALs ──────>│ cache SPS/PPS, broadcast ──>   │
    ├─ Opus frames ─────>│ broadcast ──────────────────>   │
    ├─ Telemetry ────────>│ decode + merge + forward ──>   │
    │                     │                                │
    │                     │<── POST /watch (SDP offer) ────│
    │                     │    create str0m session         │
    │                     │──> SDP answer ────────────────>│
    │                     │    RTP (H.264 + Opus) ───────>│
    │                     │    data channel (events) ─────>│
    │                     │                                │
    │── New camera join ─>│ CameraEvent::Joined             │
    │                     │    DC: renegotiate ───────────>│
    │                     │<── DC: sdp_answer ─────────────│
    │                     │    DC: track_map ─────────────>│
```

Four async tasks run concurrently:
1. **QUIC listener** (`quic.rs`) — accepts camera connections, reads frames, sends commands
2. **WebRTC engine** (`webrtc.rs`) — manages sessions, writes RTP, sends telemetry, handles renegotiation
3. **HTTP server** (`api.rs`) — SDP exchange, REST API, camera management, static file serving
4. **Audit logger** — receives audit events, writes HMAC-signed entries to stdout

## Modules

| Module | Purpose |
|--------|---------|
| `main` | CLI parsing, `AppState` creation, task spawning |
| `quic` | QUIC listener, per-camera handler, frame reader, command sender, audit instrumentation |
| `webrtc` | str0m session lifecycle, NAL accumulation, RTP output, ICE-lite, dynamic renegotiation |
| `api` | Axum routes, Bearer auth middleware, camera management, audit events, static file fallback |
| `metrics` | Prometheus metrics: gauges (cameras, sessions), counters (frames, bytes), text encoding |

Shared modules from `ghostcam` lib: `router` (GroupRouter, broadcast, SPS/PPS cache, telemetry state, command channels, camera events), `rtp` (packetizer, timestamp math), `data_channel` (message types), `telemetry` (SparseTelemetry decode/merge), `command` (CameraCommand types), `audit` (AuditEvent/AuditLogger with HMAC-SHA256).

## API Endpoints

All endpoints require `Authorization: Bearer <api-key>` except health checks.

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/v1/watch/{group_id}` | SDP offer → `{session_id, sdp_answer}` |
| `DELETE` | `/api/v1/session/{id}` | Tear down WebRTC session |
| `POST` | `/api/v1/session/{id}/ice` | Trickle ICE candidate |
| `GET` | `/api/v1/groups` | List groups with camera counts |
| `GET` | `/api/v1/groups/{group_id}/cameras` | List cameras in group |
| `GET` | `/api/v1/cameras/{device_id}/status` | Camera health, connection info, telemetry |
| `PUT` | `/api/v1/cameras/{device_id}/group` | Reassign camera to different group |
| `POST` | `/api/v1/cameras/{device_id}/command` | Send CameraCommand (fire-and-forget, 202) |
| `GET` | `/healthz` | Health check (no auth) |
| `GET` | `/readyz` | Readiness probe (no auth) |
| `GET` | `/metrics` | Prometheus metrics (no auth) |

## Data Channel Messages

JSON text over the `"telemetry"` data channel (bidirectional).

**Bridge → Viewer:**

| Type | When | Payload |
|------|------|---------|
| `cameras` | Session created | Full camera list |
| `camera_join` | Camera connects | Single camera info |
| `camera_leave` | Camera disconnects | `device_id` |
| `telemetry` | Camera sends telemetry frame | CPU, temp, memory, uptime, GPS |
| `track_map` | Data channel opens or after renegotiation | Maps SDP mid → device_id + kind |
| `renegotiate` | Camera join/leave triggers track changes | SDP offer for dynamic renegotiation |

**Viewer → Bridge:**

| Type | When | Payload |
|------|------|---------|
| `sdp_answer` | Response to `renegotiate` | Viewer's SDP answer completing renegotiation |

## Key Design Decisions

- **NAL accumulation**: Non-VCL NALs (SPS/PPS/SEI) are buffered and sent together with the next VCL NAL as Annex-B. str0m handles RTP packetization (STAP-A/FU-A).
- **ICE-lite**: Bridge acts as ICE-lite peer with a single host candidate. No STUN/TURN needed for localhost.
- **Broadcast channel**: All camera frames go through a `tokio::sync::broadcast` (capacity 4096). Slow viewers drop frames.
- **Shared state**: `Arc<AppState>` with `RwLock<GroupRouter>`. Lock scopes kept minimal.
- **Dynamic renegotiation**: When cameras join/leave, the server sends a new SDP offer via the data channel. The viewer answers, and new tracks become active without reconnecting. Events are serialized (one pending offer at a time, with queuing).
- **Audit logging**: All API and QUIC events emit `AuditEvent`s through a broadcast channel. The `AuditLogger` writes HMAC-SHA256 signed entries to stdout for tamper-evident logging.
- **Prometheus metrics**: Camera count, session count, frame/byte counters exposed at `/metrics`.

## Logging

```bash
RUST_LOG=server=debug,str0m=warn cargo run -p server -- --public-ip 127.0.0.1
```

Default filter: `server=info,str0m=warn`.

## Tests

```bash
cargo test -p server
```

Tests cover RTP timestamp conversion and H.264 FU-A packetization.
