# Ghostcam

Camera surveillance system. Cameras capture video via `rpicam-vid | ffmpeg`, upload MPEG-TS segments to S3 (Tigris), and the server generates HLS manifests on the fly. Segment requests are served via 302 redirect to S3 (re-presigned per request).

## Architecture

```
Camera (rpicam-vid | ffmpeg) → MPEG-TS segments → S3 (Tigris)
                                                      ↓
Server (Go) ← 302 redirect → Browser (hls.js)
     ↓
  Postgres (segments, users, cameras, billing)
  Redis (telemetry streams, SSE pub/sub)
```

- **No persistent connections** -- cameras POST telemetry every 10s, upload segments via presigned PUT URLs
- **Stateless server** -- JWT auth, horizontally scalable
- **S3-native HLS** -- manifests generated on the fly, segments served via 302 redirect to S3 (re-presigned per request)
- **Billing always on** -- users default to free tier (5 GB / 1 camera), dev admin gets pro tier

## Quick Start

```bash
# Set your LAN IP
echo "GHOSTCAM_PUBLIC_IP=$(ipconfig getifaddr en0)" > .env

# Start everything (server + test cameras + UI)
docker compose --profile test up -d

# Open http://localhost:5173
# Login: admin@ghostcam.dev / dev-password
```

## Project Structure

```
cmd/ghostcam-server/     Server entrypoint
cmd/ghostcam-camera/     Camera entrypoint
api/                     Shared Go types (camera <-> server contract)
camera/                  Camera: capture pipeline, S3 upload, telemetry, provisioning, gpsd
server/                  Server: HTTP handlers, DB, Redis, S3, auth, billing
  auth/                  Argon2id passwords, JWT, HMAC
  billing/               Tier definitions (free/starter/pro/enterprise)
  db/                    PostgreSQL (pgx), migrations
  handlers/              HTTP handlers (chi)
  redis/                 Telemetry streams (XADD/XREAD), pub/sub
  s3/                    Presigned URL generation (GET + PUT)
ui/                      Svelte 5 SPA (hls.js, Leaflet, Tailwind)
pi/                      Pi system files (systemd, GPS, NetworkManager)
scripts/                 Developer tools (pi.sh)
```

## Camera

The camera binary spawns `rpicam-vid | ffmpeg` to produce 6-second MPEG-TS segments, watches the output directory, and uploads new segments to S3 via presigned PUT URLs.

Key features:
- **Capture pipeline auto-restart** with exponential backoff (1s to 30s cap)
- **Upload retry queue**: 3 retries with exponential backoff (2s, 4s, 8s)
- **Graceful shutdown**: WaitGroup drain with 15s timeout
- **Segment watcher**: backpressure with 5s blocking send instead of instant drop
- **ffmpeg cleanup**: SIGTERM to process group, then SIGKILL after 5s
- **GPS**: real gpsd integration on Linux (localhost:2947 for SIM7600G-H modem), synthetic fallback in Docker/dev
- **Motion detection**: segment-size heuristic (1.5x rolling 10-segment average)
- **Telemetry poll backoff** on failure: 10s -> 30s -> 60s
- **Credentials** written as 0600 permissions
- **Segment numbering**: `-segment_start_number` avoids filename collisions on restart

```bash
# Cross-compile for Pi (3 seconds, no sysroot needed)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ghostcam-camera ./cmd/ghostcam-camera

# Deploy to Pi
scp ghostcam-camera pi@10.0.0.229:/usr/local/bin/
```

Cameras are provisioned via QR code or pre-provisioned token file. On first boot, the camera scans a QR containing the server URL + one-time token, provisions itself, and starts streaming.

## Server

HTTP API serving HLS manifests, presigned S3 URLs, SSE telemetry, and the static UI.

Key features:
- **Rate limiting**: login 10/min, register 5/min, provision 10/min per IP
- **Secure cookies**: conditional on `GHOSTCAM_PUBLIC_URL` being HTTPS
- **QR codes** use configured `GHOSTCAM_PUBLIC_URL` instead of `r.Host`
- **Storage limits**: Redis `INCRBY` atomic reservation prevents TOCTOU race
- **Storage capped events** deduplicated (5 min cooldown per device via Redis `SETNX`)
- **Admin auth** required for `/api/v1/audit` and `/api/v1/admin/reload`
- **HTTP timeouts**: Read 30s, Write 60s, Idle 120s
- **SSE**: write deadline disabled for long-lived connections
- **ListSegments**: LIMIT 2000, max 24hr range validation
- **CORS** middleware for `GHOSTCAM_PUBLIC_URL` + `localhost:5173`
- **Failed login logging** with email + IP
- **HLS manifest**: `#EXT-X-INDEPENDENT-SEGMENTS` tag; segments via 302 redirect to S3
- **Auto-create** "free" subscription on user registration

```bash
go build -o ghostcam-server ./cmd/ghostcam-server
```

### Key Endpoints

| Endpoint | Auth | Description |
|----------|------|-------------|
| `POST /api/v1/cameras/provision` | Public (rate limited) | Camera provisioning |
| `POST /api/v1/cameras/:id/telemetry` | Camera | Telemetry POST, returns pending commands |
| `POST /api/v1/cameras/:id/presign` | Camera | Presigned S3 URLs + confirm uploads |
| `GET /hls/:id/playlist.m3u8` | Viewer | Dynamic HLS manifest (max 24h, LIMIT 2000) |
| `GET /hls/:id/:segmentID.ts` | Viewer | 302 redirect to S3 (re-presigned per request) |
| `GET /hls/:id/coverage` | Viewer | Segment coverage with motion flags |
| `GET /events` | Viewer | SSE stream (telemetry, motion, storage_capped) |
| `GET /api/v1/billing/subscription` | Viewer | Always returns `billing_enabled: true` |
| `POST /api/v1/cameras` | Viewer | Returns 402 when tier camera limit reached |

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

## Infrastructure

- **Fly.io** -- server hosting (ord)
- **Tigris** -- S3-compatible object storage (edge-cached)
- **Neon** -- Postgres (us-west-2)
- **Upstash** -- Redis (sjc)

## Pi Deployment

```bash
./scripts/pi.sh setup    # First-time Pi provisioning
./scripts/pi.sh deploy   # Build + deploy camera binary
./scripts/pi.sh logs     # Stream camera logs
./scripts/pi.sh status   # Health check
```

Requires `ffmpeg` on the Pi (`apt install ffmpeg`).
