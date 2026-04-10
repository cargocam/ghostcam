# CLAUDE.md — Ghostcam Development Guide

## Documentation Policy

When making changes to the codebase, **always update the relevant READMEs, docs/, and CLAUDE.md** to reflect those changes. Detailed reference docs live in `docs/` — keep them in sync with the code.

## What is this project?

Ghostcam is a camera surveillance system built in Go. Cameras capture H.264 video + AAC audio via `rpicam-vid | ffmpeg`, upload MPEG-TS segments to S3 (Tigris) via presigned URLs, and POST telemetry over HTTP. The server generates HLS manifests on the fly, serves segment requests via 302 redirects to S3, and exposes a REST + SSE API consumed by a Svelte 5 browser viewer.

## Repository Layout

```
ghostcam/
├── common/          Shared Go types: telemetry datagrams, presign/provision contracts
├── camera/          Camera agent: capture pipeline, upload, telemetry, provisioning, gpsd, firmware
├── cmd/
│   ├── ghostcam-server/   Server entrypoint
│   └── ghostcam-camera/   Camera entrypoint
├── server/          Server: HTTP handlers (chi), DB, Redis, S3 presign, auth, billing
│   ├── auth/        Argon2id passwords, JWT, HMAC token hashing
│   ├── billing/     Tier definitions and storage limit enforcement
│   ├── db/          PostgreSQL (pgx), migrations, record types
│   ├── handlers/    HTTP handlers for all API endpoints (including events management)
│   ├── redis/       Telemetry streams (XADD/XREAD), pub/sub for SSE, event storage
│   ├── s3/          S3/Tigris presigned URL generation (GET + PUT)
│   └── ctxutil/     Context key helpers
├── ui/              Svelte 5 SPA: HLS playback (hls.js), timeline scrubber, GPS map
├── pi/              Pi system files: systemd services, GPS, NetworkManager configs
│   └── image/       rpi-image-gen build system: device configs, layer, files for flashable .img
├── scripts/         Developer tools: pi.sh (camera manager CLI)
├── docs/            Detailed reference: API, architecture, configuration, debugging
├── Dockerfile       Multi-stage: server, camera (synthetic sensors), camera-prod (real sensors)
└── docker-compose.yml  Server + UI + MinIO + Stripe webhook listener + 3 test cameras (--profile test)
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
cd ui && bun run test    # vitest unit tests
cd ui && bun run test:e2e  # playwright e2e tests (requires dev server)
```

### Testing

**Go** (`go test ./...`): Table-driven tests for pure functions. No mocking framework — tests cover:
- `server/handlers/`: `effectiveTier()` billing logic, `epochMsToISO8601` formatting, `TestCameraLimitAllowed` (tier-based camera upload enforcement)
- `server/billing/`: `GetTier()` tier resolution, `StorageLimitBytes()` computation
- `camera/`: motion detector (file-size fallback, rolling window), MPEG-TS sync byte validation, pending confirms persistence, config helpers (`coalesceStr`, `resolveVideoProfile`, `trimString`)

**UI** (`bun run test`): Vitest unit tests in `ui/src/lib/__tests__/`:
- Coverage merge logic (gap threshold, motion promotion, overlap handling)
- Alert deduplication (upsert vs append by type+cameraId)
- Time formatting (`formatTimeAgo`)

**CI** (`.github/workflows/ci.yml`): Runs `go vet`, `go test`, `bun run check`, `bun run test`, `bun run build`, Docker build on every push/PR.

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
- **build-camera**: cross-compiles camera binary for aarch64 and x86_64
- **build-camera-deb**: packages aarch64 binary as `.deb` (depends: ffmpeg, ca-certificates)
- **build-pi-image**: builds flashable `.img` for zero2w, pi4, pi5 using `rpi-image-gen`, sets device-specific video profile
- **build-docker**: builds and pushes server Docker image to GHCR
- **release**: attaches binaries, `.deb`, `.img.xz`, and checksums to the GitHub Release

## Key Ports

- `3000/tcp` — HTTP API + static viewer
- `5173/tcp` — Vite dev server (proxies `/api`, `/hls`, `/events` → :3000)

## Architecture

The server is a stateless HTTP API (Go/chi). Cameras upload MPEG-TS segments directly to S3 via presigned PUT URLs and POST telemetry over HTTP. Viewers stream HLS from the server, which generates manifests on the fly and serves segment requests via 302 redirects to S3 (re-presigning on each request to avoid mid-stream URL expiry).

```
Camera (rpicam-vid | ffmpeg) → MPEG-TS segments → S3 (Tigris)
                                                      ↓
Server (Go) ← presigned URLs → Browser (hls.js)
     ↓
  Postgres (segments, users, cameras, billing)
  Redis (telemetry streams, SSE pub/sub, events)
```

- **No persistent connections** -- cameras POST telemetry every 10s, upload segments via presigned PUT URLs
- **Stateless server** -- JWT auth, no sessions table, horizontally scalable
- **S3-native** -- segments served directly from Tigris edge via 302 redirect, no proxy
- **Single-instance deployment** -- one server behind Fly.io, one Postgres, one Redis (not designed for horizontal scaling)

For detailed subsystem documentation see:
- **[docs/usage.md](docs/usage.md)** — Camera setup (flash/deb/binary) and viewer walkthrough: enrolling, playback, clips, billing
- **[docs/api.md](docs/api.md)** — API endpoints, SSE events, camera-server protocol
- **[docs/architecture.md](docs/architecture.md)** — Camera, server, and viewer file-by-file structure
- **[docs/configuration.md](docs/configuration.md)** — Environment variables, config files, billing tiers, background jobs
- **[docs/debugging.md](docs/debugging.md)** — Troubleshooting common issues

## Code Conventions

### Go

- **Error handling**: Return `error` from functions, wrap with `fmt.Errorf("context: %w", err)`.
- **Logging**: `log/slog` with structured fields: `slog.Info("connected", "device_id", id)`.
- **HTTP**: chi router. Handlers are methods on `Handlers` struct. JSON responses via `writeJSON()`.
- **Database**: pgx v5 pool. Database interface for testability. Batch inserts via `pgx.Batch`.
- **Concurrency**: `sync.WaitGroup` for goroutine lifecycle, `sync/atomic` for flags, channels for inter-goroutine communication.
- **Build tags**: `//go:build linux && !synthetic` for real sensors (gpsd, /proc, nmcli). `//go:build !linux || synthetic` for synthetic sensors. Docker camera target uses `-tags synthetic`. Production Pi builds use real sensors with no synthetic code.

### Svelte / TypeScript

- **Svelte 5 runes only**: `$state`, `$derived`, `$effect`, `$props()`. No legacy `$:`.
- **Stores**: Exported object literals with `$state` fields — not class-based.
- **Styling**: Tailwind CSS 4 utility classes. OKLCH tokens in `app.css`. `cn()` for merging.
- **Components**: bits-ui primitives in `components/ui/`. Domain components alongside views.
- **localStorage**: Keys prefixed with `ghostcam-`.

## Key Dependencies

| Package | Notes |
|---------|-------|
| `github.com/go-chi/chi/v5` | HTTP router |
| `github.com/jackc/pgx/v5` | PostgreSQL driver (connection pool) |
| `github.com/redis/go-redis/v9` | Redis client (Streams, pub/sub) |
| `github.com/aws/aws-sdk-go-v2` | S3/Tigris presigned URLs |
| `github.com/BurntSushi/toml` | Config file parsing |
| `github.com/google/uuid` | UUID generation for segment IDs |
| `golang.org/x/crypto/argon2` | Password hashing (Argon2id) |
| `svelte` (5) | Frontend. Runes: `$state`, `$derived`, `$effect` |
| `tailwindcss` (4) | OKLCH color system, `@import "tailwindcss"` |
| `hls.js` (1) | HLS playback in browser |
| `@ffmpeg/ffmpeg` (0.11.6) | Client-side MP4 assembly via ffmpeg.wasm (lazy-loaded) |
| `bits-ui` (2) | Headless component primitives |
| `leaflet` (1.9) | Map |
| `qrcode` (npm) | Client-side QR SVG generation for enrollment |
