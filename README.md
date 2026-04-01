# Ghostcam

Camera surveillance system. Cameras capture video via `rpicam-vid | ffmpeg`, upload MPEG-TS segments to S3, and the server generates HLS manifests on the fly. Viewers stream directly from S3.

## Architecture

```
Camera (rpicam-vid | ffmpeg) → MPEG-TS segments → S3 (Tigris)
                                                      ↓
Server (Go) ← presigned URLs → Browser (hls.js)
     ↓
  Postgres (segments, users, cameras)
  Redis (telemetry streams, SSE)
```

- **No persistent connections** — cameras POST telemetry every 10s, upload segments via presigned PUT URLs
- **Stateless server** — JWT auth, no sessions table, horizontally scalable
- **S3-native** — segments served directly from Tigris edge, no proxy

## Quick Start

```bash
# Set your LAN IP (required for S3 presigned URLs in dev)
echo "GHOSTCAM_PUBLIC_IP=$(ipconfig getifaddr en0)" > .env

# Start everything (server + 2 test cameras + UI)
docker compose --profile test up -d

# Open http://localhost:5173
# Login: admin@ghostcam.dev / dev-password
```

## Project Structure

```
cmd/ghostcam-server/     Server entrypoint
cmd/ghostcam-camera/     Camera entrypoint
api/                     Shared API types (camera ↔ server contract)
camera/                  Camera: capture, upload, telemetry, provisioning
server/                  Server: HTTP handlers, DB, Redis, S3, auth
  db/migrations/         SQL migrations (Postgres)
  handlers/              HTTP handlers (chi)
  redis/                 Telemetry streams (XADD/XREAD)
  s3/                    Presigned URL generation
  auth/                  Argon2id passwords, JWT, HMAC
ui/                      Svelte 5 SPA (hls.js, Leaflet, Tailwind)
pi/                      Pi system files (systemd, GPS, NetworkManager)
scripts/                 Developer tools (pi.sh)
```

## Camera

The camera binary spawns `rpicam-vid | ffmpeg` to produce 6-second MPEG-TS segments, watches the output directory, and uploads new segments to S3 via presigned PUT URLs.

```bash
# Cross-compile for Pi (3 seconds, no sysroot needed)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ghostcam-camera ./cmd/ghostcam-camera

# Deploy to Pi
scp ghostcam-camera pi@10.0.0.229:/usr/local/bin/
```

Cameras are provisioned via QR code or pre-provisioned token file. On first boot, the camera scans a QR containing the server URL + one-time token, provisions itself, and starts streaming.

## Server

HTTP API serving HLS manifests, presigned S3 URLs, SSE telemetry, and the static UI.

```bash
go build -o ghostcam-server ./cmd/ghostcam-server
```

### Key Endpoints

| Endpoint | Auth | Description |
|----------|------|-------------|
| `POST /api/v1/cameras/provision` | Public | Camera provisioning |
| `POST /api/v1/cameras/:id/telemetry` | Camera | Telemetry + commands |
| `POST /api/v1/cameras/:id/presign` | Camera | Presigned S3 URLs |
| `GET /hls/:id/playlist.m3u8` | Viewer | Dynamic HLS manifest |
| `GET /events` | Viewer | SSE telemetry stream |
| `GET /api/v1/telemetry/:id` | Viewer | Historical telemetry |

## Infrastructure

- **Fly.io** — server hosting (Seattle)
- **Tigris** — S3-compatible object storage (edge-cached)
- **Neon** — Postgres
- **Upstash** — Redis

## Pi Deployment

```bash
./scripts/pi.sh setup    # First-time Pi provisioning
./scripts/pi.sh deploy   # Build + deploy camera binary
./scripts/pi.sh logs     # Stream camera logs
./scripts/pi.sh status   # Health check
```

Requires `ffmpeg` on the Pi (`apt install ffmpeg`).
