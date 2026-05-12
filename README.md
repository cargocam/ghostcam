# Ghostcam

Self-hosted camera surveillance for Raspberry Pi. Cameras capture H.264 + AAC
with `rpicam-vid | ffmpeg`, upload MPEG-TS segments directly to S3, and a
single Go server handles auth, HLS, and live WebRTC for the Svelte browser
viewer.

## Architecture

```
Camera (rpicam-vid) ── raw H.264 ──┬── ffmpeg ──┬── MPEG-TS ──→ S3 (Tigris)
                                   │            └── Opus ──┐
                                   └── H.264 via WebSocket ┤
                                                           ↓
Server (Go)  ←── presigned URLs ──→  Browser (hls.js)   pion SFU
     │                                                     ↓
     ├─ Postgres (users, cameras, segments, billing)   Browser (WebRTC)
     └─ Redis    (telemetry streams, SSE, events)
```

- **Hybrid HLS + WebRTC** — recording is always HLS (segments on S3, manifests
  generated on the fly, segments served via 302 redirect). Live viewing uses a
  pion SFU (ICE-lite, no TURN) for sub-second latency, with automatic fallback
  to HLS.
- **On-demand relay** — cameras keep a persistent WebSocket but only push
  H.264/Opus frames when a viewer is watching.
- **Ed25519 identity** — each camera mints a permanent keypair on first boot;
  device ID is `SHA-256(pubkey)[:16]`. Requests are signed per-call, so
  switching servers only requires a new provision token.
- **Stateless HTTP path** — JWT auth, no session table. The WebRTC SFU keeps
  in-memory state and pins live viewing to one server instance; HLS scales
  independently.
- **No cleanup daemons** — segment retention is enforced opportunistically in
  the presign handler (DB row + S3 object deleted together, bounded per call).

## Quick start

```bash
echo "GHOSTCAM_PUBLIC_IP=$(ipconfig getifaddr en0)" > .env
docker compose --profile test up -d
# http://localhost:5173  —  admin@ghostcam.dev / dev-password
```

`--profile test` brings up the server, UI, MinIO, Stripe webhook listener, and
three synthetic cameras. Omit it to run just the server + UI when developing
against real Pi hardware.

For camera provisioning, the viewer walkthrough, and the Pi dev loop see
**[docs/usage.md](docs/usage.md)**.

## Layout

```
common/     Shared Go types — camera<->server wire contract
camera/     Camera binary (package main): capture, upload, telemetry, QR provisioning, gpsd
server/     Server binary (package main): chi router, handlers as methods on *App
  apitypes/   Viewer<->server request/response/SSE types (tygo source)
  auth/       Argon2id, JWT, HMAC token hashing
  billing/    Tier definitions and storage limits
  db/         Postgres (pgx), migrations
  mailer/     Resend transactional email
  redis/      Telemetry streams, pub/sub, events
  s3/         Tigris presigned URLs
  triage/     Anthropic classifier for inbound support email
  linear/     Linear issueCreate client
ui/         Svelte 5 SPA (hls.js, Leaflet, Tailwind 4)
  src/lib/api-types/   Generated from tygo — do not edit
e2e/        Playwright specs that drive the live compose stack
pi/         systemd, GPS, NetworkManager configs + rpi-image-gen build
scripts/    pi.sh — build/deploy/logs loop for real hardware
docs/       API, architecture, configuration, debugging
```

Both `camera/` and `server/` are top-level `package main` — no `cmd/` wrapper.

## Build

```bash
go build -o ghostcam-server ./server

# Build the Python camera wheel
cd camera && python -m build --wheel    # output: dist/ghostcam-*.whl

# Regenerate TypeScript + pydantic types after editing common/ or server/apitypes/
go generate ./...

# Tests
go test ./...
cd camera && pytest -q           # 71 unit + parity tests for the Python camera
cd ui && bun run test            # vitest unit tests
cd ui && bun run test:browser    # playwright smoke (backend mocked)
cd e2e && bun run test           # real end-to-end against compose stack
```

CI runs `go vet`, `go test`, the tygo drift check, `bun run check`, `bun run
build`, the Docker build, and the e2e suite on every push.

## Releases

Push a `v*` tag. The [release workflow](.github/workflows/release.yml) produces:

| Artifact | Description |
|----------|-------------|
| `ghostcam-camera-{aarch64,x86_64}` | Standalone Linux binaries |
| `ghostcam-camera_<version>_arm64.deb` | Debian package (ffmpeg + ca-certificates deps) |
| `ghostcam-{zero2w,pi4,pi5}-<version>.img.xz` | Flashable Pi images |
| `checksums.txt` | SHA-256 for every artifact |

A GitHub webhook (`POST /api/v1/webhooks/github`) mirrors the `.img.xz` assets
into S3 at `firmware/{version}/ghostcam-{device}.img.xz` and publishes them
via `GET /api/v1/firmware/images` for the UI's onboarding flow.

## Infrastructure

- **Fly.io** — server (sjc)
- **Tigris** — S3-compatible object storage, edge-cached
- **Neon** — Postgres (us-west-2)
- **Upstash** — Redis (sjc)

## Further reading

- **[docs/usage.md](docs/usage.md)** — camera setup (image / deb / binary) and viewer walkthrough
- **[docs/api.md](docs/api.md)** — HTTP endpoints, SSE events, camera-server protocol
- **[docs/architecture.md](docs/architecture.md)** — file-by-file structure
- **[docs/configuration.md](docs/configuration.md)** — env vars, billing tiers, retention
- **[docs/debugging.md](docs/debugging.md)** — troubleshooting
