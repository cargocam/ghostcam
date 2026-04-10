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

## Project Structure

```
cmd/ghostcam-server/     Server entrypoint
cmd/ghostcam-camera/     Camera entrypoint
common/                  Shared Go types (camera <-> server contract)
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
- **Pre-encoded test loop**: place `test-loop.mp4` in data dir for low-CPU test mode (~5% vs 49%)
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
- **Admin auth** required for `/api/v1/audit` and `/api/v1/admin/reload`
- **HTTP timeouts**: Read 30s, Write 60s, Idle 120s
- **SSE**: write deadline disabled for long-lived connections
- **ListSegments**: LIMIT 2000, max 24hr range validation
- **CORS** middleware for `GHOSTCAM_PUBLIC_URL` + `localhost:5173`
- **Failed login logging** with email + IP
- **HLS manifest**: `#EXT-X-INDEPENDENT-SEGMENTS` tag; segments via 302 redirect to S3
- **Auto-create** "free" subscription on user creation
- **Background jobs**: hourly segment retention (30 days default), 6-hourly stale camera cleanup

```bash
go build -o ghostcam-server ./cmd/ghostcam-server
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
| `GHOSTCAM_SEGMENT_RETENTION_DAYS` | Segment retention (default 30); hourly cleanup |

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

## Camera Setup

Three ways to set up a camera, from easiest to most flexible.

### Option 1: Flash a Pi Image (recommended for new hardware)

Download the `.img.xz` for your Pi model from the [latest release](../../releases/latest) and flash it to an SD card:

```bash
# macOS
xzcat ghostcam-zero2w-v0.1.0.img.xz | sudo dd of=/dev/diskN bs=4M

# Linux
xzcat ghostcam-zero2w-v0.1.0.img.xz | sudo dd of=/dev/sdX bs=4M status=progress
```

The image comes pre-configured with all dependencies (ffmpeg, gpsd, modemmanager, ALSA, NetworkManager) and the camera service enabled. On first boot:

1. SSH in (user: `ghostcam`, password: `ghostcam`)
2. Set the server URL: `echo "GHOSTCAM_SERVER_URL=https://your-server.example.com" >> /etc/ghostcam/env`
3. Provision the camera from the web UI (generates a one-time token), then write it: `echo "<token>" > /var/ghostcam/provision_token`
4. Restart: `sudo systemctl restart ghostcam-camera`

The camera provisions itself on the next start and begins streaming. Subsequent boots are automatic.

### Option 2: Install the .deb Package (existing Pi with Raspberry Pi OS)

For Pis already running Raspberry Pi OS (Bookworm, arm64):

```bash
# Download and install
curl -LO https://github.com/<owner>/ghostcam/releases/latest/download/ghostcam-camera_<version>_arm64.deb
sudo dpkg -i ghostcam-camera_<version>_arm64.deb

# Install remaining dependencies
sudo apt install -y rpicam-apps gpsd gpsd-clients modemmanager alsa-utils

# Create data directory
sudo mkdir -p /var/ghostcam /etc/ghostcam
sudo chown $USER:$USER /var/ghostcam

# Configure environment
sudo tee /etc/ghostcam/env << EOF
GHOSTCAM_DATA_DIR=/var/ghostcam
GHOSTCAM_SERVER_URL=https://your-server.example.com
GHOSTCAM_VIDEO_PROFILE=pi4
EOF

# Install and enable the systemd service
sudo cp pi/systemd/ghostcam-camera.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ghostcam-camera
```

Set `GHOSTCAM_VIDEO_PROFILE` to match your hardware: `zero2w` (480p), `pi4` (720p), or `pi5` (1080p).

Provision the camera the same way as Option 1 (write a provision token, or use `pi.sh setup` for full automated provisioning).

### Option 3: Deploy the Raw Binary (any Linux/arm64 or amd64)

Download the standalone binary from the [latest release](../../releases/latest). This is useful for non-Pi Linux systems or custom setups:

```bash
curl -LO https://github.com/<owner>/ghostcam/releases/latest/download/ghostcam-camera-aarch64
chmod +x ghostcam-camera-aarch64
sudo mv ghostcam-camera-aarch64 /usr/local/bin/ghostcam-camera

# Requires ffmpeg on PATH
sudo apt install -y ffmpeg

# Create data directory
sudo mkdir -p /var/ghostcam
sudo chown $USER:$USER /var/ghostcam

# Run with environment variables
GHOSTCAM_SERVER_URL=https://your-server.example.com \
GHOSTCAM_DATA_DIR=/var/ghostcam \
GHOSTCAM_VIDEO_PROFILE=pi4 \
  ghostcam-camera
```

For production use, set up a systemd service (see `pi/systemd/ghostcam-camera.service` as a template).

## Pi Development

```bash
./scripts/pi.sh setup    # First-time Pi provisioning (installs deps, deploys config + binary)
./scripts/pi.sh deploy   # Build + deploy camera binary
./scripts/pi.sh logs     # Stream camera logs
./scripts/pi.sh status   # Health check
```
