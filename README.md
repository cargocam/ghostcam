# Ghostcam

Camera surveillance system. Cameras capture video via `rpicam-vid | ffmpeg`, upload MPEG-TS segments to S3 (Tigris), and the server generates HLS manifests on the fly. Segment requests are served via 302 redirect to S3 (re-presigned per request).

## Architecture

```
Camera (rpicam-vid | ffmpeg) → MPEG-TS segments → S3 (Tigris)
                                                      ↓
Server (Go) ← 302 redirect → Browser (hls.js)
     ↓
  Postgres (segments, users, cameras, billing)
  Redis (telemetry streams, SSE pub/sub, events)
```

- **No persistent connections** -- cameras POST telemetry every 10s, upload segments via presigned PUT URLs
- **Stateless server** -- JWT auth, horizontally scalable
- **S3-native HLS** -- manifests generated on the fly, segments served via 302 redirect to S3 (re-presigned per request)
- **Single-instance deployment** -- designed for one server behind Fly.io, not horizontally scaled
- **Billing always on** -- users default to free tier (5 GB / 1 camera); Stripe test mode for local dev
- **Clip download** -- timeline range selection, client-side MP4 assembly via ffmpeg.wasm, telemetry CSV/JSON export

## Quick Start

```bash
# Set your LAN IP
echo "GHOSTCAM_PUBLIC_IP=$(ipconfig getifaddr en0)" > .env

# Start everything (server + test cameras + UI)
docker compose --profile test up -d

# Open http://localhost:5173
# Login: admin@ghostcam.dev / dev-password
```

For camera setup and a full walkthrough of the viewer, see **[docs/usage.md](docs/usage.md)**.

## Project Structure

```
common/                  Shared Go types (camera <-> server contract)
camera/                  Camera binary (package main): capture pipeline, S3 upload,
                         telemetry, provisioning, gpsd
server/                  Server binary (package main): chi router + HTTP handlers
                         (methods on *App), plus subpackages:
  apitypes/              Viewer<->server request/response/SSE types (tygo source)
  auth/                  Argon2id passwords, JWT, HMAC
  billing/               Tier definitions (free/starter/pro/enterprise)
  db/                    PostgreSQL (pgx), migrations
  redis/                 Telemetry streams (XADD/XREAD), pub/sub
  s3/                    Presigned URL generation, Upload, Delete
tools/                   Go build-time tool pins (tygo)
tygo.yaml                Codegen: common/ + server/apitypes/ -> ui/src/lib/api-types/
ui/                      Svelte 5 SPA (hls.js, Leaflet, Tailwind)
  src/lib/api-types/     Generated TypeScript types — DO NOT EDIT (tygo output)
pi/                      Pi system files (systemd, GPS, NetworkManager)
scripts/                 Developer tools (pi.sh)
```

Both `camera/` and `server/` are top-level `package main` directories — no
`cmd/` wrapper. `go build ./camera` and `go build ./server` produce the two
binaries.

## Camera

The camera binary spawns `rpicam-vid | ffmpeg` to produce 6-second MPEG-TS segments, watches the output directory, and uploads new segments to S3 via presigned PUT URLs.

Key features:
- **Capture pipeline auto-restart** with exponential backoff (1s to 30s cap)
- **Upload retry queue**: 3 retries with exponential backoff (2s, 4s, 8s)
- **Graceful shutdown**: WaitGroup drain with 15s timeout
- **Segment watcher**: backpressure with 5s blocking send instead of instant drop
- **ffmpeg cleanup**: SIGTERM to process group, then SIGKILL after 5s
- **GPS**: real gpsd integration on Linux (localhost:2947 for SIM7600G-H modem), synthetic fallback in Docker/dev
- **Pre-encoded test loop**: place `test-loop.mp4` in data dir for low-CPU test mode (~5% vs 49%)
- **Motion detection**: segment-size heuristic (1.5x rolling 10-segment average)
- **Telemetry poll backoff** on failure: 10s -> 30s -> 60s
- **Credentials** written as 0600 permissions
- **Segment numbering**: `-segment_start_number` avoids filename collisions on restart

```bash
# Cross-compile for Pi (3 seconds, no sysroot needed)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ghostcam-camera ./camera

# Deploy to Pi
scp ghostcam-camera pi@10.0.0.229:/usr/local/bin/
```

Cameras are provisioned via pre-provisioned token file. On first boot, the camera reads the token file containing the server URL + one-time token, provisions itself, and starts streaming. (Camera-side QR scanning is not implemented in Go.)

## Server

HTTP API serving HLS manifests, presigned S3 URLs, SSE telemetry, and the static UI.

Key features:
- **Registration disabled**: `POST /api/v1/auth/register` returns 403; admin creates users via DB
- **Firmware OTA**: admin uploads via `POST /api/v1/admin/firmware` (Tigris), cameras auto-update on startup
- **Rate limiting**: login 10/min, provision 10/min per IP
- **Secure cookies**: conditional on `GHOSTCAM_PUBLIC_URL` being HTTPS
- **QR codes** use configured `GHOSTCAM_PUBLIC_URL` instead of `r.Host`
- **Storage limits**: Redis `INCRBY` atomic reservation prevents TOCTOU race
- **Storage capped events** deduplicated (5 min cooldown per device via Redis `SETNX`)
- **Admin auth** required for `/api/v1/admin/firmware`
- **HTTP timeouts**: Read 30s, Write 60s, Idle 120s
- **SSE**: write deadline disabled for long-lived connections
- **ListSegments**: LIMIT 2000, max 24hr range validation
- **CORS** middleware for `GHOSTCAM_PUBLIC_URL` + `localhost:5173`
- **Failed login logging** with email + IP
- **HLS manifest**: `#EXT-X-INDEPENDENT-SEGMENTS` tag; segments via 302 redirect to S3
- **Auto-create** "free" subscription on user creation
- **No cleanup daemons**: segment retention is enforced by opportunistic
  prune in the presign handler — DB rows and their matching S3 objects
  are deleted together (LIMIT 100 per call), tied to normal upload
  activity. Firmware binaries share the bucket and are intentionally
  excluded.
- **Fail-closed billing**: `billing.GetTier` returns `(Tier, bool)`;
  unknown tier strings never grant unlimited resources. `effectiveTier`
  validates DB-stored tiers and Stripe webhooks refuse to escalate to a
  paid tier on unrecognised price IDs.

```bash
go build -o ghostcam-server ./server
```

### Key Endpoints

| Endpoint | Auth | Description |
|----------|------|-------------|
| `POST /api/v1/cameras/provision` | Public (rate limited) | Camera provisioning |
| `POST /api/v1/cameras/:id/telemetry` | Camera | Telemetry POST, returns pending commands |
| `POST /api/v1/cameras/:id/presign` | Camera | Presigned S3 URLs + confirm uploads |
| `GET /hls/:id/live.m3u8` | Viewer | Live HLS manifest (90s sliding window) |
| `GET /hls/:id/vod.m3u8` | Viewer | VOD HLS manifest (?from=&to=, max 24h) |
| `GET /hls/:id/:segmentID.ts` | Viewer | 302 redirect to S3 (re-presigned per request) |
| `GET /hls/:id/coverage` | Viewer | Segment coverage with motion flags |
| `POST /api/v1/clips/prepare` | Viewer | Presigned segment URLs for clip download |
| `GET /api/v1/telemetry/:id/export` | Viewer | Telemetry export (CSV/JSON) |
| `GET /events` | Viewer | SSE stream (telemetry, motion, storage_capped, coverage) |
| `GET /api/v1/events` | Viewer | List events with pagination |
| `GET /api/v1/billing/subscription` | Viewer | Always returns `billing_enabled: true` |
| `POST /api/v1/cameras` | Viewer | Returns 402 when tier camera limit reached |
| `POST /api/v1/admin/firmware` | Admin | Upload firmware binary to Tigris |
| `GET /api/v1/firmware/latest` | Public | Latest firmware version + presigned download URL |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `GHOSTCAM_DATABASE_URL` | PostgreSQL connection string (required) |
| `GHOSTCAM_REDIS_URL` | Redis URL for telemetry + SSE |
| `GHOSTCAM_PUBLIC_URL` | Public URL for QR codes and CORS (e.g. `https://cam.example.com`) |
| `GHOSTCAM_S3_BUCKET` | S3/Tigris bucket name |
| `GHOSTCAM_S3_ENDPOINT` | S3 endpoint URL (Tigris, MinIO) |
| `GHOSTCAM_ADMIN_EMAIL` | Admin user email |
| `GHOSTCAM_ADMIN_PASSWORD` | Preset admin password |
| `GHOSTCAM_SEGMENT_RETENTION_DAYS` | Segment retention (default 30); drives the opportunistic prune in the presign handler and the read cutoff for manifest/coverage queries |

## Infrastructure

- **Fly.io** -- server hosting (sjc)
- **Tigris** -- S3-compatible object storage (edge-cached)
- **Neon** -- Postgres (us-west-2)
- **Upstash** -- Redis (sjc)

## Releases

Releases are triggered by pushing a Git tag (`v*`). The [release workflow](.github/workflows/release.yml) produces:

| Artifact | Description |
|----------|-------------|
| `ghostcam-camera-aarch64` | Standalone Linux/arm64 binary |
| `ghostcam-camera-x86_64` | Standalone Linux/amd64 binary |
| `ghostcam-camera_<version>_arm64.deb` | Debian package (arm64) |
| `ghostcam-<device>-<version>.img.xz` | Flashable Pi OS image (zero2w, pi4, pi5) |
| `checksums.txt` | SHA-256 checksums for all artifacts |

```bash
git tag v0.1.0
git push origin v0.1.0
```

## Camera Setup & Usage

For camera setup (flashing Pi images, installing the .deb, running the raw binary), Pi developer workflow, and a walkthrough of the viewer (enrolling cameras, playback, clip downloads, billing), see **[docs/usage.md](docs/usage.md)**.
