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
├── server/          Server binary: QUIC ingest, WebRTC egress, HTTP API, Redis telemetry, PostgreSQL
├── ui/              Svelte 5 SPA: live WebRTC view, HLS playback, timeline scrubber, GPS map
├── pi/              Pi system files: systemd services, GPS, NetworkManager configs
├── scripts/         Developer tools: pi.sh (camera manager CLI)
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

### Local dev

All services run through docker-compose. Never run server, cameras, or UI natively.
In dev, Vite serves the UI with HMR. In production, the Rust server serves the built static files directly (no separate UI process).

```bash
# Copy .env.example and fill in your LAN IP (required for Firefox WebRTC):
cp .env.example .env
# macOS: echo "GHOSTCAM_PUBLIC_IP=$(ipconfig getifaddr en0)" >> .env
# Linux: echo "GHOSTCAM_PUBLIC_IP=$(hostname -I | awk '{print $1}')" >> .env
# Optionally add Stripe keys for billing (see .env.example)

docker compose build
```

Two workflows depending on whether you're using test cameras or real hardware:

**Server/UI development (test cameras, no hardware)**:
```bash
docker compose up -d --profile test   # server + UI + 3 test cameras
# Open http://localhost:5173  (login: admin@ghostcam.dev / dev-password)
```

**Camera firmware on real Pi hardware**:
```bash
docker compose up -d                  # server + UI only (no test cameras)
./scripts/pi.sh deploy                # cross-compile, deploy to Pi, tail logs
```

The Pi camera connects to the Docker server via the host's LAN IP (`GHOSTCAM_PUBLIC_IP`). Both workflows can run simultaneously -- test cameras and real hardware connect to the same server.

```bash
# Camera manager CLI (all Pi operations):
./scripts/pi.sh setup    [HOST] [USER] [PASS]   # First-time Pi provisioning
./scripts/pi.sh deploy   [HOST] [USER] [PASS]   # Build + deploy (primary dev loop)
./scripts/pi.sh logs     [HOST] [USER] [PASS]   # Stream camera logs
./scripts/pi.sh status   [HOST] [USER] [PASS]   # Health check
./scripts/pi.sh wifi-off [SECS] [HOST] [USER] [PASS]  # Cellular failover test
./scripts/pi.sh restart  [HOST] [USER] [PASS]   # Restart camera service
./scripts/pi.sh ssh      [HOST] [USER] [PASS]   # Interactive SSH
./scripts/pi.sh unenroll [HOST] [USER] [PASS]   # Reset enrollment

# Defaults configured via .pi.env (gitignored) or CLI args
# Clean restart: docker compose down -v && docker compose up -d
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

## Configuration

Both server and camera support TOML config files with layered resolution. Environment variables and CLI flags always take precedence. Config files are **optional** -- the env-var-only workflow still works (Docker uses this).

### Layering Order

**Server**: defaults -> config file -> env vars
**Camera**: defaults -> config file -> env vars -> CLI flags

### Config File Search Paths

**Server** (first found wins):
1. `$GHOSTCAM_CONFIG_FILE`
2. `$GHOSTCAM_DATA_DIR/server.toml`
3. `/etc/ghostcam/server.toml`

**Camera** (first found wins):
1. `--config <path>` CLI flag
2. `$GHOSTCAM_CONFIG_FILE`
3. `$GHOSTCAM_DATA_DIR/camera.toml`
4. `/boot/ghostcam.conf` (backward compatible -- valid TOML key=value format)

### Example Files

- `server.example.toml` -- all server knobs with comments
- `camera.example.toml` -- all camera knobs with comments

### Sensitive Fields

`database_url` and `admin_password` are **env-var only** (`#[serde(skip)]`). They cannot be set in the TOML config file.

### Runtime Reload

The server supports reloading config without restart:
- **SIGHUP**: `kill -HUP <pid>` re-reads the config file
- **API**: `POST /api/v1/admin/reload` (requires auth)

Settings that require restart (ports, database_url, data_dir) log warnings on reload but don't take effect until restart.

### Key Environment Variables

| Variable | Binary | Default | Description |
|----------|--------|---------|-------------|
| `GHOSTCAM_CONFIG_FILE` | both | _(none)_ | Explicit config file path |
| `GHOSTCAM_DATA_DIR` | both | `/var/ghostcam` | Data directory |
| `GHOSTCAM_DATABASE_URL` | server | _(required)_ | PostgreSQL URL |
| `GHOSTCAM_REDIS_URL` | server | _(none)_ | Redis URL |
| `GHOSTCAM_PUBLIC_IP` | server | _(none)_ | ICE candidate IP |
| `GHOSTCAM_HTTP_PORT` | server | `3000` | HTTP port |
| `GHOSTCAM_QUIC_PORT` | server | `4433` | QUIC port |
| `GHOSTCAM_WEBRTC_PORT` | server | `3478` | WebRTC UDP port |
| `GHOSTCAM_ENROLLMENT_ADDR` | server | `<public_ip>:<quic_port>` | Enrollment JWT address |
| `GHOSTCAM_ADMIN_EMAIL` | server | `admin@localhost` | Admin email |
| `GHOSTCAM_ADMIN_PASSWORD` | server | _(auto-generated)_ | Preset admin password |
| `GHOSTCAM_SERVER_ADDR` | camera | _(from enrollment)_ | Server QUIC address |
| `GHOSTCAM_AUDIO_DEVICE` | camera | _(system default)_ | ALSA audio input device name |
| `STRIPE_SECRET_KEY` | server | _(none)_ | Stripe API key (billing enabled if set) |
| `STRIPE_WEBHOOK_SECRET` | server | _(none)_ | Stripe webhook signing secret |
| `STRIPE_PRICE_ID_STARTER` | server | _(none)_ | Stripe Price ID for starter tier |
| `STRIPE_PRICE_ID_PRO` | server | _(none)_ | Stripe Price ID for pro tier |
| `STRIPE_PRICE_ID_ENTERPRISE` | server | _(none)_ | Stripe Price ID for enterprise tier |
| `STRIPE_PORTAL_CONFIG_ID` | server | _(none)_ | Portal config with plan switching |

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
config.rs     Port/size/limit constants (QUIC_PORT, HTTP_PORT, BROADCAST_CAPACITY, QUIC_MAX_*, MAX_REQUEST_BODY_BYTES, MAX_SESSIONS_PER_USER, TELEMETRY_BATCH_INTERVAL_SECS, ...) + helpers: load_toml(), env_or(), env_opt()
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
config.rs        CameraConfig + CameraConfigFile, layered TOML/env/CLI resolution
session.rs       Active QUIC session: alerts stream, command gate atomics
enrollment.rs    JWT enrollment handshake
tofu.rs          Server fingerprint pinning (first connect)
quic.rs          QUIC endpoint with mTLS device cert
commands.rs      CameraCommand handler → updates watch channels
capture/         Test sources: video_test.rs (loop H.264), audio_test/ (synthetic Opus)
                 Real capture: video.rs (rpicam-vid), audio.rs (cpal+opus, Linux only)
stream/          Frame senders: video.rs, audio.rs (write to persistent QUIC streams)
recording/       fMP4 ring buffer: muxer, segment, ring_buffer, manifest, uploads
telemetry/       sensors.rs (/proc, /sys, gpsd), buffer.rs (batch upload)
```

## Server Structure

```
main.rs       Config load, PKI bootstrap, task spawning, SIGHUP reload, Axum bind
config.rs     ServerConfig + ServerConfigFile, layered TOML/env resolution
db_trait.rs   Database trait + record types (CameraRecord, SessionRecord, ApiTokenRecord, AuditLogRecord, SubscriptionRecord, UsageRecord, ...)
db.rs         PostgresDatabase — sqlx PostgreSQL implementation
auth.rs       Token hashing, HMAC, session validation
audit.rs      AuditLogger — HMAC-SHA256 signed audit trail (file + DB dual-write, wired into AppState)
sse.rs        SseEventBus — per-user broadcast for Server-Sent Events
billing/      Stripe integration, subscription tiers, camera limit enforcement, grace period background task
api/          Axum routes, rate limiting (see server/src/api/README.md)
ingest/       QUIC accept loop, IngestSlot, RoutingRegistry (see server/src/ingest/README.md)
egress/       EgressHandle, SessionManager, RTP packetizer (see server/src/egress/README.md)
pki/          CA bootstrap, device enrollment, revocation (see server/src/pki/README.md)
redis/        Telemetry write/query via Redis Streams, ConnectionManager, TelemetryBatcher, usage counters (see server/src/redis/README.md)
```

## Viewer Structure

```
auth.ts                Login/logout/session
sse.ts                 SSE client — camera_online/offline events drive WebRTC session lifecycle
connection-manager.ts  SSE event → WebRTC session orchestration
signaling.ts           watchCamera/unwatchCamera (WebRTC SDP), fetchTelemetryRangeCached
webrtc.ts              RTCPeerConnection per camera; stripCandidates() strips mDNS for Firefox
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
  billing.svelte.ts       Subscription, tiers, usage state + Stripe checkout/portal
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
POST   /api/v1/auth/register               { email, password, display_name? } → 201 + session cookie
POST   /api/v1/auth/login                  { email, password } → session cookie
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

GET    /api/v1/billing/subscription         Current subscription + tier info
GET    /api/v1/billing/tiers               Available tiers with pricing
POST   /api/v1/billing/checkout            { tier, success_url, cancel_url } → { url }
POST   /api/v1/billing/portal              { return_url } → { url }
GET    /api/v1/billing/usage               Current month usage stats
POST   /api/v1/webhooks/stripe             Stripe webhook (public, signature-verified)

GET    /api/v1/audit                       Audit log query (?type=&since=&until=&limit=&offset=)
POST   /api/v1/admin/reload                Reload config from disk

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
- **Audit log**: Set `GHOSTCAM_HMAC_KEY` to a secret key for HMAC-SHA256 signing (default: `dev-hmac-key`). Entries are written to `{data_dir}/audit.jsonl` and the `audit_log` PostgreSQL table. Query via `GET /api/v1/audit`.
- **str0m API**: Pinned at 0.6.x. Key methods: `Rtc::builder().set_ice_lite(true)`, `sdp_api().accept_offer(offer)`, `rtc.writer(mid)`, `channel.write(binary, data)`.
- **Billing disabled**: If `STRIPE_SECRET_KEY` is unset, billing is fully disabled — unlimited cameras, no payment UI. The `/api/v1/billing/subscription` endpoint returns `{ billing_enabled: false, tier: "unlimited" }`.
- **Camera limit 402**: When billing is enabled and a user hits their camera limit, `POST /api/v1/cameras` returns HTTP 402 with `{ error: "camera_limit_reached" }`.
- **Billing webhooks**: Stripe webhooks keep subscription state in sync. In production, set `STRIPE_WEBHOOK_SECRET` to the real signing secret. In local dev, run `docker compose --profile stripe up stripe-webhooks` to forward events via the Stripe CLI container — it prints the `whsec_...` secret to stdout on startup.
- **Stripe Portal plan switching**: Requires `STRIPE_PORTAL_CONFIG_ID` — create one via the Stripe API or Dashboard with `subscription_update.enabled=true` and the product/price list. Without it, the portal only shows cancellation.

## Key Dependencies

| Crate/Package | Version | Notes |
|---------------|---------|-------|
| `quinn` | 0.11 | QUIC transport |
| `str0m` | 0.6 | Sans-I/O WebRTC, ICE-lite |
| `axum` | 0.7 | HTTP framework |
| `rustls` | 0.23 | TLS for QUIC |
| `rcgen` | 0.13 | Cert generation. `KeyPair::generate()`, `CertificateParams::self_signed(&kp)` |
| `sqlx` | 0.8 | PostgreSQL async |
| `redis` | 0.27 | Redis Streams for telemetry (with `connection-manager` feature) |
| `async-stripe` | 0.39 | Stripe billing (checkout, portal, webhooks). Optional — no key = free tier |
| `governor` | 0.10 | Token-bucket rate limiting |
| `argon2` | 0.5 | Password hashing |
| `rmp-serde` | 1 | MessagePack for telemetry wire format |
| `toml` | 0.8 | Config file parsing |
| `tokio` | 1 | Async runtime |
| `svelte` | 5 | Frontend. Runes: `$state`, `$derived`, `$effect` |
| `tailwindcss` | 4 | OKLCH color system, `@import "tailwindcss"` |
| `hls.js` | 1 | HLS playback in browser |
| `bits-ui` | 2 | Headless component primitives |
| `leaflet` | 1.9 | Map |
