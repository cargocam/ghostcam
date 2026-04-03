# CLAUDE.md — Ghostcam Development Guide

## Documentation Policy

When making changes to the codebase, **always update the relevant READMEs and CLAUDE.md** to reflect those changes. This includes wire protocol messages, API endpoints, CLI flags, architecture, data flow, viewer features, dependencies, and build instructions. Each crate and major subsystem has its own README — keep them in sync with the code.

## What is this project?

Ghostcam is a camera surveillance system built in Go. Cameras capture H.264 video + AAC audio via `rpicam-vid | ffmpeg`, upload MPEG-TS segments to S3 (Tigris) via presigned URLs, and POST telemetry over HTTP. The server generates HLS manifests on the fly, serves segment requests via 302 redirects to S3, and exposes a REST + SSE API consumed by a Svelte 5 browser viewer.

## Repository Layout

```
ghostcam/
├── api/             Shared Go types: telemetry datagrams, presign/provision contracts
├── camera/          Camera agent: capture pipeline, upload, telemetry, provisioning, gpsd
├── cmd/
│   ├── ghostcam-server/   Server entrypoint
│   └── ghostcam-camera/   Camera entrypoint
├── server/          Server: HTTP handlers (chi), DB, Redis, S3 presign, auth, billing
│   ├── auth/        Argon2id passwords, JWT, HMAC token hashing
│   ├── billing/     Tier definitions and storage limit enforcement
│   ├── db/          PostgreSQL (pgx), migrations, record types
│   ├── handlers/    HTTP handlers for all API endpoints
│   ├── redis/       Telemetry streams (XADD/XREAD), pub/sub for SSE events
│   ├── s3/          S3/Tigris presigned URL generation (GET + PUT)
│   └── ctxutil/     Context key helpers
├── ui/              Svelte 5 SPA: HLS playback (hls.js), timeline scrubber, GPS map
├── pi/              Pi system files: systemd services, GPS, NetworkManager configs
│   └── image/       rpi-image-gen build system: device configs, layer, files for flashable .img
├── scripts/         Developer tools: pi.sh (camera manager CLI)
├── Dockerfile       Multi-stage: server + camera targets
├── docker-compose.yml
└── .github/workflows/ci.yml
```

## Build & Run

```bash
# Build server
go build -o ghostcam-server ./cmd/ghostcam-server

# Build camera
go build -o ghostcam-camera ./cmd/ghostcam-camera

# Cross-compile camera for Pi (no CGO needed)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ghostcam-camera ./cmd/ghostcam-camera

# Run tests
go test ./...
```

### Local dev

All services run through docker-compose. Never run server, cameras, or UI natively.
In dev, Vite serves the UI with HMR. In production, the Go server serves the built static files directly (no separate UI process).

```bash
# Set your LAN IP
echo "GHOSTCAM_PUBLIC_IP=$(ipconfig getifaddr en0)" > .env

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
- **go**: `go vet ./...`, `go test ./...`
- **ui**: `bun install --frozen-lockfile`, `bun run check`, `bun run build`
- **docker**: builds both server and camera targets with BuildKit cache

`.github/workflows/release.yml` — triggers on tags (`v*`):
- **build-camera-deb**: cross-compiles camera binary for aarch64, packages as `.deb`
- **build-pi-image**: builds flashable `.img` for zero2w, pi4, pi5 using `rpi-image-gen`
- **release**: attaches `.img.xz` files to the GitHub Release

## Key Ports

- `3000/tcp` — HTTP API + static viewer
- `5173/tcp` — Vite dev server (proxies `/api`, `/hls`, `/events` → :3000)

## Configuration

Both server and camera support TOML config files with layered resolution. Environment variables and CLI flags always take precedence. Config files are **optional** -- the env-var-only workflow still works (Docker uses this). Server is written in Go (chi router, pgx for Postgres).

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

### Sensitive Fields

`database_url` and `admin_password` are **env-var only**. They cannot be set in the TOML config file.

### Runtime Reload

- **API**: `POST /api/v1/admin/reload` (requires admin auth) — reloads config from disk

### Key Environment Variables

| Variable | Binary | Default | Description |
|----------|--------|---------|-------------|
| `GHOSTCAM_CONFIG_FILE` | both | _(none)_ | Explicit config file path |
| `GHOSTCAM_DATA_DIR` | both | `/var/ghostcam` | Data directory |
| `GHOSTCAM_DATABASE_URL` | server | _(required)_ | PostgreSQL URL |
| `GHOSTCAM_REDIS_URL` | server | _(none)_ | Redis URL (telemetry streams, SSE pub/sub) |
| `GHOSTCAM_HTTP_PORT` | server | `3000` | HTTP port |
| `GHOSTCAM_ADMIN_EMAIL` | server | `admin@localhost` | Admin email |
| `GHOSTCAM_ADMIN_PASSWORD` | server | _(auto-generated)_ | Preset admin password |
| `GHOSTCAM_PUBLIC_URL` | server | _(none)_ | Public URL for QR codes and CORS origin (e.g. `https://cam.example.com`) |
| `GHOSTCAM_S3_BUCKET` | server | `ghostcam-segments` | S3/Tigris bucket name |
| `GHOSTCAM_S3_REGION` | server | `auto` | S3 region |
| `GHOSTCAM_S3_ENDPOINT` | server | _(none)_ | S3 endpoint URL (Tigris, MinIO, etc.) |
| `GHOSTCAM_S3_PRESIGN_TTL_SECS` | server | `3600` | Presigned URL TTL in seconds |
| `GHOSTCAM_HMAC_KEY` | server | `dev-hmac-key` | HMAC key for audit log signing |
| `GHOSTCAM_SERVER_URL` | camera | _(from provisioning)_ | Server HTTPS URL |
| `GHOSTCAM_AUDIO_DEVICE` | camera | `default` | ALSA audio input device name |
| `GHOSTCAM_LOCAL_STORAGE_CAP_MB` | camera | `4096` | Local segment storage cap in MB; oldest segments evicted when exceeded |
| `GHOSTCAM_VIDEO_PROFILE` | camera | _(none)_ | Video preset: `zero2w`/`480p`, `pi4`/`720p`, `pi5`/`1080p` |
| `STRIPE_SECRET_KEY` | server | _(none)_ | Stripe API key (checkout/portal integration) |
| `STRIPE_WEBHOOK_SECRET` | server | _(none)_ | Stripe webhook signing secret |
| `STRIPE_PRICE_ID_STARTER` | server | _(none)_ | Stripe Price ID for starter tier |
| `STRIPE_PRICE_ID_PRO` | server | _(none)_ | Stripe Price ID for pro tier |
| `STRIPE_PRICE_ID_ENTERPRISE` | server | _(none)_ | Stripe Price ID for enterprise tier |
| `STRIPE_PORTAL_CONFIG_ID` | server | _(none)_ | Portal config with plan switching |
| `GHOSTCAM_SEGMENT_RETENTION_DAYS` | server | `30` | Segment retention in days; segments older than this are deleted hourly |
| `GHOSTCAM_RELEASE_REPO` | server | _(none)_ | GitHub `owner/repo` for firmware releases |
| `GITHUB_WEBHOOK_SECRET` | server | _(none)_ | GitHub webhook HMAC secret |

## Background Jobs

The server runs several background goroutines:

| Job | Interval | Description |
|-----|----------|-------------|
| Session cleanup | 1 hour | Deletes expired sessions |
| Segment retention | 1 hour | Deletes segments older than `GHOSTCAM_SEGMENT_RETENTION_DAYS` (default 30) from S3 and Postgres, 100 at a time |
| Stale camera cleanup | 6 hours | Deletes unclaimed cameras older than 24h and expired provision tokens |

## Architecture

The server is a stateless HTTP API (Go/chi). Cameras upload MPEG-TS segments directly to S3 via presigned PUT URLs and POST telemetry over HTTP. Viewers stream HLS from the server, which generates manifests on the fly and serves segment requests via 302 redirects to S3 (re-presigning on each request to avoid mid-stream URL expiry).

```
Camera (rpicam-vid | ffmpeg) → MPEG-TS segments → S3 (Tigris)
                                                      ↓
Server (Go) ← presigned URLs → Browser (hls.js)
     ↓
  Postgres (segments, users, cameras, billing)
  Redis (telemetry streams, SSE pub/sub)
```

- **No persistent connections** -- cameras POST telemetry every 10s, upload segments via presigned PUT URLs
- **Stateless server** -- JWT auth, no sessions table, horizontally scalable
- **S3-native** -- segments served directly from Tigris edge via 302 redirect, no proxy

### Provisioning (QR Code)

Cameras provision themselves via a one-time token, optionally delivered via QR code:

1. User generates a provision token from the web UI (`POST /api/v1/cameras` or `GET /api/v1/cameras/enroll/qr`).
2. QR payload is `{"s": "https://server-url", "t": "<token>", "w": "ssid", "p": "password"}` with WiFi fields optional.
3. Camera reads token from pre-provisioned file, POSTs to `POST /api/v1/cameras/provision` with the token + device serial. (Camera-side QR scanning is not implemented in Go; provision via token files only.)
4. Server validates token, creates camera record, returns API key + device ID.
5. Camera persists credentials (`api_key`, `device_id`, `server_url`) as flat files (0600 permissions) and starts streaming.
6. On subsequent boots, camera loads stored credentials and starts immediately.

**QR codes use `GHOSTCAM_PUBLIC_URL`** for the server address instead of `r.Host`, ensuring correct URLs behind reverse proxies.

### Camera Upload Flow

1. Camera requests presigned PUT URLs in batches (`POST /api/v1/cameras/{id}/presign`), confirming previously uploaded segments in the same request.
2. Camera uploads MPEG-TS segments directly to S3 using the presigned PUT URL.
3. Server records segment metadata (start/end timestamps, size, motion flag) in Postgres.
4. Upload retry queue: 3 retries with exponential backoff (2s, 4s, 8s). Failed segments stay on disk.
5. `storageCapped` flag (atomic.Bool) pauses uploads when server indicates storage limit reached.

### HLS Playback

1. Browser requests `/hls/{deviceID}/playlist.m3u8?from=&to=` -- server queries Postgres for segment metadata, builds manifest with relative `.ts` paths and `#EXT-X-INDEPENDENT-SEGMENTS` tag.
2. hls.js fetches segments via `/hls/{deviceID}/{segmentID}.ts` -- server presigns a GET URL on the fly and returns 302 redirect to S3.
3. No presigned URLs embedded in manifests -- each segment request is re-presigned, avoiding mid-stream URL expiry.
4. `ListSegments` query: `LIMIT 2000`, max 24-hour time range validation.

### Billing (Always On)

Billing is always enabled. Every user defaults to the **free** tier (5 GB storage, 1 camera). The dev admin account is automatically created with a **pro** tier subscription. Tier enforcement:

- **Camera limit**: `POST /api/v1/cameras` returns HTTP 402 `camera_limit_reached` when the user's tier camera limit is reached.
- **Storage limit**: The presign handler uses Redis `INCRBY` for atomic reservation to prevent TOCTOU race conditions when checking storage limits. If over limit, returns `storage_capped: true`.
- **Storage capped events**: Deduplicated per device with a 5-minute cooldown via Redis `SETNX`.
- **Registration disabled**: `POST /api/v1/auth/register` returns 403. Admin users are seeded on first run via env vars.

Tiers: Free (5 GB / 1 camera), Starter (50 GB / 4 cameras), Pro (500 GB / 16 cameras), Enterprise (unlimited).

### SSE Event Types

The `/events` endpoint delivers the following event types via Redis pub/sub:

| Event | Payload | Description |
|-------|---------|-------------|
| `telemetry` | `{ device_id, telemetry }` | Realtime telemetry from Redis Streams (XREAD) |
| `motion_detected` | `{ device_id, segment_id, start_ts, end_ts }` | Motion detected in a recording segment |
| `storage_capped` | `{ user_id, device_id, storage_bytes, limit_gb }` | User's storage exceeds tier limit; uploads paused |

SSE connections use `http.NewResponseController` to disable the write deadline for long-lived connections.

## Shared API Types (`api/`)

```
types.go       PresignRequest, PresignResponse, PresignedUrl, UploadedSegment, ProvisionRequest/Response
               CameraCommand — server→camera commands delivered via telemetry poll response
               (set_resolution, set_recording_mode, reboot, unregister, network_config, remove_network)
telemetry.go   TelemetryDatagram — JSON payload with optional fields (CPU, temp, mem, GPS, wifi signal, uptime)
```

## Camera Structure

```
cmd/ghostcam-camera/
  main.go          Entrypoint, task orchestration (WaitGroup), capture crash recovery with exponential backoff (1s→30s),
                   telemetry poll loop with failure backoff (10s→30s→60s), graceful shutdown (WaitGroup drain, 15s timeout)

camera/
  config.go        CameraConfig + cameraConfigFile, layered TOML/env/CLI resolution
                   RecordingMode ("constant"/"motion") — runtime override via {dataDir}/recording_mode
                   LocalStorageCapBytes — configurable via GHOSTCAM_LOCAL_STORAGE_CAP_MB (default 4096 MB)
                   Resolution runtime override via {dataDir}/resolution
                   Video profiles: zero2w/480p, pi4/720p, pi5/1080p
  capture.go       Capture pipeline: rpicam-vid | ffmpeg → MPEG-TS segments (6s each)
                   Test mode: ffmpeg testsrc2 + sine audio, or pre-encoded test-loop.mp4 (~5% CPU vs 49%)
                   Uses -segment_start_number to avoid filename collisions on restart
                   ffmpeg cleanup: SIGTERM to process group, then SIGKILL after 5s
  watcher.go       NewSegment type, motionDetector (rolling 10-window, 1.5x threshold)
                   RunSegmentWatcher: polls every 2s, skips 0-byte and still-being-written files
                   Backpressure: 5s blocking send to segment channel (drops if full)
                   EnforceLocalStorageCap: evicts oldest .ts files when over cap
  upload.go        RunUploadLoop: consumes segments from channel, uploads via presigned PUT URLs
                   Retry queue: 3 retries with exponential backoff (2s, 4s, 8s)
                   storageCapped: atomic.Bool — pauses uploads when server indicates storage full
                   Graceful shutdown: flushes pending confirmations with 5s timeout
  client.go        HTTP client for server API (telemetry POST, presign, provision, S3 upload)
  credentials.go   LoadCredentials / SaveCredentials — flat files (api_key, device_id, server_url) with 0600 permissions
  provisioning.go  Token-based provisioning via POST /api/v1/cameras/provision
                   Note: QR scanning is not implemented in Go. Cameras provision via token files only.
  sensors_linux.go ReadTelemetry: CPU (/proc/stat), memory (/proc/meminfo), temp (/sys/class/thermal),
                   uptime (/proc/uptime), WiFi signal (/proc/net/wireless), GPS (gpsd → synthetic fallback)
  sensors_other.go Synthetic telemetry for non-Linux (dev/Docker)
  sensors_common.go  InjectSyntheticGPS: deterministic GPS from device serial hash (for dev/Docker)
  gpsd.go          (Linux) Real gpsd integration: connects to localhost:2947, enables JSON watch,
                   reads TPV reports for lat/lon/alt/fix. Used by SIM7600G-H modem on Pi.
  gpsd_other.go    (non-Linux) No-op gpsd stub
  network_linux.go EnsureWifi (nmcli), WaitForRoute (/proc/net/route polling)
  network_other.go No-op stubs for non-Linux
```

Runtime override files (`{dataDir}/resolution`, `{dataDir}/recording_mode`) are written when the server sends `set_resolution` or `set_recording_mode` commands via the telemetry poll response. The camera reads these on startup and on pipeline restart, allowing remote configuration changes to survive reboots.

## Server Structure

```
cmd/ghostcam-server/
  main.go         Entrypoint: config load, DB connect, Redis/S3 init, HTTP server with timeouts
                  HTTP timeouts: Read 30s, Write 60s, Idle 120s
                  Graceful shutdown with 10s timeout
                  Hourly expired session cleanup

server/
  app.go          App struct: DB, Redis, S3, HMACSecret, Config
  config.go       ServerConfig + serverConfigFile, layered TOML/env resolution
                  PublicURL for QR codes and CORS origin
  routes.go       Chi router: route groups, rate limiting, CORS middleware
                  Rate limits: login 10/min, register 5/min, provision 10/min per IP
                  CORS: allows PublicURL + localhost:5173
                  Secure cookies: conditional on PublicURL being HTTPS
  middleware.go   ViewerAuth (JWT cookie + Bearer API token), CameraAuth (Bearer API key),
                  AdminAuth (viewer auth + admin email check for /api/v1/audit and /api/v1/admin/reload)

  auth/           Argon2id password hashing, JWT sign/verify, HMAC token hashing, random password generation
  billing/        Tier definitions: Free (5 GB / 1 camera), Starter (50 GB / 4 cameras),
                  Pro (500 GB / 16 cameras), Enterprise (unlimited)
  db/             PostgreSQL via pgx v5 — connection pool, migrations, record types
                  Database interface for testability
  handlers/       HTTP handlers for all API endpoints
                  handlers.go: defaultTierID = "free" (billing always on)
                  admin.go: FirmwareUpload (POST /api/v1/admin/firmware), FirmwareLatest (GET /api/v1/firmware/latest)
                  auth.go: Login (failed login logging with email + IP), Register (returns 403 — disabled)
                  cameras.go: Enroll (camera limit 402), UpdateCamera (enqueues commands for resolution/recording_mode)
                  hls.go: GetManifest (dynamic m3u8 with #EXT-X-INDEPENDENT-SEGMENTS), GetSegment (302 redirect to S3),
                          GetInit (302 redirect), GetCoverage (segment list with motion flags)
                  presign.go: Storage limit check with Redis INCRBY atomic reservation,
                              storage_capped event deduplication (5 min cooldown via Redis SETNX)
                  sse.go: SSE via Redis XREAD + pub/sub, write deadline disabled for long-lived connections
                  qr.go: QR code generation using PublicURL (not r.Host)
                  provision.go: Camera provisioning with one-time token
  redis/          Telemetry write (XADD) and query (XREAD), pub/sub for motion_detected and storage_capped events
  s3/             S3/Tigris client, presigned GET/PUT URL generation, key helpers
  ctxutil/        Context key helpers (UserID, CameraDeviceID, CameraUserID)
```

### Database Migrations

| Migration | Description |
|-----------|-------------|
| `001_initial.sql` | Users, cameras, sessions, API tokens, segments |
| `002_multi_user.sql` | Multi-user support |
| `003_audit_log.sql` | Audit log table |
| `004_billing.sql` | Subscriptions table |
| `005_fk_cascade.sql` | Foreign key cascades |
| `006_ownership.sql` | Camera ownership |
| `007_hls_rewrite.sql` | HLS rewrite: provision tokens, commands queue, camera API keys, segment has_motion |
| `008_motion.sql` | Adds `has_motion` boolean column to `segments` table |

## Viewer Structure

```
signaling.ts           API calls: fetchCameras, fetchTelemetryRange, fetchCoverage
stores/
  transport.svelte.ts  SSE connection, auth state, camera polling
  cameras.svelte.ts    Camera registry, telemetry, online status
  scrubber.svelte.ts   Timeline mode (live/playback), playhead time, coverage data
  settings.svelte.ts   Theme, layout, mute state (localStorage)
  groups.svelte.ts     Group list + active group
  alerts.svelte.ts     Disconnect/reconnect notifications; handles motion and storage_capped alert types
  cameraConfig.svelte.ts  Display name overrides (localStorage)
  billing.svelte.ts       Subscription, tiers, usage state + Stripe checkout/portal
                          Derived fields: storageUsedGB, storageLimitGB, storagePercent, isStorageCapped
components/
  HlsPlayer.svelte    hls.js wrapper for HLS playback via /hls/{deviceID}/playlist.m3u8
  TimelineScrubber.svelte  Timeline with coverage bars, playhead, live/playback mode switching
  camera/CameraCard.svelte  Camera card with HLS player, settings dialog
  camera/CameraList.svelte  Sidebar camera list with right-click context menu (Rename, Delete)
  camera/CameraSettingsDialog.svelte  Camera settings: Name, Resolution, Recording Mode
```

## Camera-Server Protocol

All communication is plain HTTPS (no QUIC, no WebRTC). Cameras authenticate with a Bearer API key obtained during provisioning.

### Telemetry Poll (camera → server, every 10s)

`POST /api/v1/cameras/{deviceID}/telemetry` with JSON `TelemetryDatagram` body. Response contains an array of `CameraCommand` objects (piggy-backed commands).

### CameraCommand (server → camera, via telemetry response)

JSON objects with a `"type"` field: `set_resolution { resolution }`, `set_recording_mode { mode }`, `reboot`, `unregister`, `network_config { ssid, psk }`, `remove_network { ssid }`

`set_resolution` and `set_recording_mode` are persisted to disk by the camera (`{dataDir}/resolution`, `{dataDir}/recording_mode`) and trigger a process exit (systemd restarts with new config).

### Segment Upload (camera → S3 → server confirmation)

1. Camera requests presigned PUT URLs: `POST /api/v1/cameras/{deviceID}/presign` with `{ count, uploaded[] }`
2. Camera uploads MPEG-TS segments directly to S3 via presigned PUT URL
3. Uploaded segment metadata is confirmed in the next presign request's `uploaded[]` array
4. Server records segments in Postgres and publishes motion events via Redis

### Telemetry Datagram

JSON-encoded with optional fields. Sent every 10s. Fields: `ts` (unix ms), `cpu` (%), `mem` (MB), `temp` (C), `uptime` (s), `lat`, `lon`, `alt`, `gps_fix`, `sig` (WiFi dBm).

## API Quick Reference

Auth: `Authorization: Bearer <token>` or `ghostcam-token=<jwt>` cookie. Cookies use `Secure` flag when `GHOSTCAM_PUBLIC_URL` starts with `https://`.

```
POST   /api/v1/auth/register               DISABLED (returns 403 registration_disabled)
POST   /api/v1/auth/login                  { email, password } → JWT cookie (rate limited: 10/min per IP)
POST   /api/v1/auth/logout                 Clears JWT cookie
PATCH  /api/v1/auth/password               { current_password, new_password }

GET    /api/v1/cameras                     List user's claimed cameras
POST   /api/v1/cameras                     Generate provision token (returns 402 when tier camera limit reached)
POST   /api/v1/cameras/enroll/qr           QR code (PNG) with provision token + optional WiFi
GET    /api/v1/cameras/enroll/qr           Same as POST with defaults (24h TTL, no WiFi)
GET    /api/v1/cameras/:id                 Camera details
PATCH  /api/v1/cameras/:id                 Update name/notes/resolution/recording_mode
DELETE /api/v1/cameras/:id                 Delete camera

POST   /api/v1/cameras/:id/telemetry       Camera telemetry POST (camera auth) → returns pending commands
POST   /api/v1/cameras/:id/presign         Request presigned S3 URLs + confirm uploads (camera auth)
POST   /api/v1/cameras/provision            Camera provisioning with one-time token (rate limited: 10/min per IP)

GET    /api/v1/telemetry/:id/latest        Most recent telemetry from Redis
GET    /api/v1/telemetry/:id               ?from=<ms>&to=<ms>&limit=<n> — historical telemetry range

GET    /hls/:id/playlist.m3u8              Dynamic HLS manifest (?from=&to=, max 24h range, LIMIT 2000 segments)
GET    /hls/:id/init.mp4                   Init segment → 307 redirect to S3
GET    /hls/:id/:segmentID.ts              Segment → 302 redirect to S3 (presigned on the fly)
GET    /hls/:id/coverage                   Segment coverage with motion flags (has_motion always present, not omitted when false)

GET    /events                             SSE stream (telemetry, motion, storage_capped events)

GET    /api/v1/tokens                      List API tokens
POST   /api/v1/tokens                      Create token
DELETE /api/v1/tokens/:id                  Revoke token

GET    /api/v1/billing/subscription         Returns { billing_enabled, tier }
GET    /api/v1/billing/tiers               Available tiers with limits (public)
POST   /api/v1/billing/checkout            { tier, success_url, cancel_url } → { url } (creates Stripe Checkout Session)
POST   /api/v1/billing/portal              { return_url } → { url } (Stripe Customer Portal)
GET    /api/v1/billing/usage               Storage + camera usage for current user
POST   /api/v1/webhooks/stripe             Stripe webhook: checkout.session.completed, subscription.updated, subscription.deleted

GET    /api/v1/firmware/latest             Latest firmware version + presigned Tigris download URL (public, no auth)
POST   /api/v1/admin/firmware             Upload firmware binary to Tigris + publish version via Redis — admin only
POST   /api/v1/webhooks/github            GitHub release webhook (public, HMAC-verified)

GET    /api/v1/audit                       Audit log query — admin only (?type=&since=&until=&limit=&offset=)
POST   /api/v1/admin/reload                Reload config from disk — admin only

GET    /healthz                            Always 200 (no auth)
GET    /readyz                             200 when ready (no auth)
```

## Code Conventions

### Go

- **Error handling**: Return `error` from functions, wrap with `fmt.Errorf("context: %w", err)`.
- **Logging**: `log/slog` with structured fields: `slog.Info("connected", "device_id", id)`.
- **HTTP**: chi router. Handlers are methods on `Handlers` struct. JSON responses via `writeJSON()`.
- **Database**: pgx v5 pool. Database interface for testability. Batch inserts via `pgx.Batch`.
- **Concurrency**: `sync.WaitGroup` for goroutine lifecycle, `sync/atomic` for flags, channels for inter-goroutine communication.
- **Build tags**: `//go:build linux` for platform-specific code (gpsd, /proc sensors, nmcli).

### Svelte / TypeScript

- **Svelte 5 runes only**: `$state`, `$derived`, `$effect`, `$props()`. No legacy `$:`.
- **Stores**: Exported object literals with `$state` fields — not class-based.
- **Styling**: Tailwind CSS 4 utility classes. OKLCH tokens in `app.css`. `cn()` for merging.
- **Components**: bits-ui primitives in `components/ui/`. Domain components alongside views.
- **localStorage**: Keys prefixed with `ghostcam-`.

## Debugging Tips

- **Telemetry API 503**: `GHOSTCAM_REDIS_URL` is unset or empty — Redis is required for telemetry history and SSE events.
- **Camera not provisioning**: Check the provision token is valid and not expired. Camera POSTs to `POST /api/v1/cameras/provision`. Rate limited to 10/min per IP.
- **Camera unclaimed**: Provision via QR code in the web UI (`GET /api/v1/cameras/enroll/qr`) or API (`POST /api/v1/cameras` to get a provision token). Pre-provision by writing `api_key`, `device_id`, `server_url` files to the camera's data dir.
- **Capture pipeline crashing**: Pipeline auto-restarts with exponential backoff (1s to 30s cap). Backoff resets after 30s of healthy operation. Check for ffmpeg availability and rpicam-vid on real hardware.
- **Segment filename collisions**: ffmpeg uses `-segment_start_number` based on existing `.ts` file count to avoid overwriting segments on camera restart.
- **GPS not working**: On Linux, the camera connects to gpsd on `localhost:2947`. Ensure gpsd is running and the SIM7600G-H modem is connected. Falls back to synthetic GPS in Docker/dev (deterministic from device serial hash).
- **Motion detection**: Segment-size-based heuristic — a segment is flagged as motion if its size exceeds 1.5x the rolling average of the last 10 segments. The `has_motion` column in the `segments` table tracks this per-segment.
- **Storage cap**: When a user exceeds their tier's storage limit, the presign handler uses Redis `INCRBY` for atomic reservation (prevents TOCTOU race), returns `storage_capped: true`, and emits a deduplicated `storage_capped` SSE event (5 min cooldown per device via Redis `SETNX`). Uploads are paused until storage is freed or the user upgrades.
- **Local storage eviction**: The camera evicts oldest segments when local storage exceeds 4 GB (default). Configure via `GHOSTCAM_LOCAL_STORAGE_CAP_MB` environment variable or `local_storage_cap_mb` in the camera config file.
- **Upload failures**: Upload retry queue with 3 retries and exponential backoff (2s, 4s, 8s). After max retries, segment stays on disk (not deleted). `storageCapped` uses `atomic.Bool` to avoid data races.
- **Audit log**: Set `GHOSTCAM_HMAC_KEY` to a secret key for HMAC-SHA256 signing (default: `dev-hmac-key`). Entries are written to the `audit_log` PostgreSQL table. Query via `GET /api/v1/audit` (admin only).
- **Billing always on**: Every user defaults to the "free" tier (5 GB, 1 camera). The dev admin gets "pro" tier. `GET /api/v1/billing/subscription` always returns `{ billing_enabled: true, tier: "<tier>" }`.
- **Camera limit 402**: When a user hits their tier's camera limit, `POST /api/v1/cameras` returns HTTP 402 with `{ error: "camera_limit_reached" }`.
- **Failed login logging**: Login failures are logged with email + IP address (via `X-Forwarded-For` or `RemoteAddr`).
- **HLS segment expiry**: Manifests use relative `.ts` paths. Each segment request to `/hls/{id}/{segmentID}.ts` re-presigns on the fly and returns 302 to S3. No presigned URLs in manifests means no mid-stream expiry.
- **Billing webhooks**: Stripe webhooks keep subscription state in sync. In production, set `STRIPE_WEBHOOK_SECRET` to the real signing secret.
- **Firmware OTA**: Admin uploads firmware via `POST /api/v1/admin/firmware` (stored in Tigris, version published via Redis). Cameras check `GET /api/v1/firmware/latest` on startup and auto-update via staged binary + systemd `ExecStartPre` swap. Set `GHOSTCAM_RELEASE_REPO` on the server to enable startup fetch from GitHub API.
- **Pre-encoded test loop**: Place `test-loop.mp4` in the camera's data dir for low-CPU test mode (~5% vs 49% with testsrc2 encoding). The camera uses `ffmpeg -stream_loop` to segment it continuously.
- **Unenroll script**: `pi.sh unenroll` clears credential files (`api_key`, `device_id`, `server_url`) from the camera's data dir.

## Key Dependencies

| Package | Notes |
|---------|-------|
| `github.com/go-chi/chi/v5` | HTTP router |
| `github.com/jackc/pgx/v5` | PostgreSQL driver (connection pool) |
| `github.com/redis/go-redis/v9` | Redis client (Streams, pub/sub) |
| `github.com/aws/aws-sdk-go-v2` | S3/Tigris presigned URLs |
| `github.com/BurntSushi/toml` | Config file parsing |
| `github.com/skip2/go-qrcode` | QR code generation (PNG) for enrollment |
| `github.com/google/uuid` | UUID generation for segment IDs |
| `golang.org/x/crypto/argon2` | Password hashing (Argon2id) |
| `svelte` (5) | Frontend. Runes: `$state`, `$derived`, `$effect` |
| `tailwindcss` (4) | OKLCH color system, `@import "tailwindcss"` |
| `hls.js` (1) | HLS playback in browser |
| `bits-ui` (2) | Headless component primitives |
| `leaflet` (1.9) | Map |
