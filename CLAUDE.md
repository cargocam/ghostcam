# CLAUDE.md — Ghostcam Development Guide

## Documentation Policy

When making changes to the codebase, **always update the relevant READMEs, docs/, and CLAUDE.md** to reflect those changes. Detailed reference docs live in `docs/` — keep them in sync with the code.

## What is this project?

Ghostcam is a camera surveillance system built in Go. Cameras capture H.264 video + AAC audio via `rpicam-vid | ffmpeg`, upload MPEG-TS segments to S3 (Tigris) via presigned URLs, and POST telemetry over HTTP. The server generates HLS manifests on the fly, serves segment requests via 302 redirects to S3, and exposes a REST + SSE API consumed by a Svelte 5 browser viewer.

## Repository Layout

```
ghostcam/
├── common/          Shared Go types: camera<->server contract (telemetry, presign, provisioning)
├── camera/          Camera binary (package main): capture pipeline, upload, telemetry,
│                    provisioning (CLI/env → flat files → QR scan), gpsd, firmware.
│                    main.go lives here — no cmd/ wrapper.
├── server/          Server binary (package main): chi router + HTTP handlers as methods
│                    on *App, middleware, rate limiting. main.go lives here — no cmd/ wrapper.
│   ├── apitypes/    Viewer<->server HTTP request/response + SSE payload types.
│   │                Source of truth for ui/src/lib/api-types/ — types only,
│   │                tygo reads this package plus common/.
│   ├── auth/        Argon2id passwords, JWT, HMAC token hashing
│   ├── mailer/      Resend transactional email (verify, reset, OTP, change-email)
│   ├── billing/     Tier definitions and storage limit enforcement
│   ├── db/          PostgreSQL (pgx), migrations, record types (concrete *DB, no interface)
│   ├── redis/       Telemetry streams (XADD/XREAD), pub/sub for SSE, event storage
│   ├── s3/          S3/Tigris presigned URL generation, Upload, Delete
│   ├── triage/      Anthropic Messages API classifier for inbound support email (no-op when ANTHROPIC_API_KEY is unset)
│   └── linear/      Minimal GraphQL client for Linear issueCreate (no-op when LINEAR_API_KEY is unset)
│                    (Inbound webhook handler lives in server/support.go alongside the other webhook handlers.)
├── tygo.yaml        Codegen config: common/ + server/apitypes/ → ui/src/lib/api-types/ (driven by `go generate ./...`)
├── ui/              Svelte 5 SPA: HLS playback (hls.js), timeline scrubber, GPS map
│   └── src/lib/api-types/  Generated TypeScript types — DO NOT EDIT (see tygo.yaml)
├── e2e/             Real end-to-end Playwright specs that drive the live
│                    docker-compose stack (not mocked). Run via CI e2e job.
├── pi/              Pi system files: systemd services, GPS, NetworkManager configs
│   └── image/       rpi-image-gen build system: device configs, layer, files for flashable .img
├── scripts/         Developer tools: pi.sh (camera manager CLI)
├── docs/            Detailed reference: API, architecture, configuration, debugging
├── Dockerfile       Multi-stage: server, camera (synthetic sensors), camera-prod (real sensors)
└── docker-compose.yml  Server + UI + MinIO + Stripe webhook listener + 3 test cameras (--profile test; stripe-webhooks runs by default)
```

## Build & Run

```bash
# Build server
go build -o ghostcam-server ./server

# Build camera
go build -o ghostcam-camera ./camera

# Cross-compile camera for Pi (no CGO needed)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ghostcam-camera ./camera

# Run tests
go test ./...
cd ui && bun run test    # vitest unit tests
cd ui && bun run test:browser  # playwright browser tests (frontend smoke; backend is mocked)

# Regenerate TypeScript API types from Go source of truth.
# Run after changing any struct in common/ or server/apitypes/.
go generate ./...
```

### API Type Generation

The UI consumes a single source of truth for every request/response/event
payload: `server/apitypes/apitypes.go` (viewer<->server) and `common/types.go`
+ `common/telemetry.go` (camera<->server). `tygo` walks those packages and
writes matching TypeScript interfaces to `ui/src/lib/api-types/`. The UI
imports exclusively from `$lib/api-types`, and the `browser-tests/` fixtures
are typed against the same file — so any drift between the Go structs and
the TypeScript consumers is a compile error, not a runtime mystery.

To change a wire shape:

1. Edit the Go struct in `server/apitypes/` or `common/`.
2. Run `go generate ./...`.
3. Commit both the Go change and the regenerated `ui/src/lib/api-types/` files.

CI runs `go generate ./... (drift check)` (via the `go` job). A PR that modifies a struct
without regenerating is hard-rejected — the drift check uses
`git diff --exit-code` against the regenerated output.

### Testing

**Go** (`go test ./...`): Table-driven tests for pure functions. No mocking framework — tests cover:
- `server/`: `effectiveTier()` billing logic, `epochMsToISO8601` formatting, `TestCameraLimitAllowed` (tier-based camera upload enforcement)
- `server/billing/`: `GetTier()` tier resolution, `StorageLimitBytes()` computation
- `camera/`: motion detector (file-size fallback, rolling window), MPEG-TS sync byte validation, pending confirms persistence, config helpers (`coalesceStr`, `resolveVideoProfile`, `trimString`), H.264 NAL parser (start code detection, IDR identification, ring buffer overflow)

**UI** (`bun run test`): Vitest unit tests in `ui/src/lib/__tests__/`:
- Coverage merge logic (gap threshold, motion promotion, overlap handling)
- Alert deduplication (upsert vs append by type+cameraId)
- Time formatting (`formatTimeAgo`)
- Storage-cap banner thresholds (warning at 85%, capped at 100%, dismissal behavior)

**UI browser tests** (`bun run test:browser`): Playwright specs in `ui/browser-tests/`
that run in real Chromium against the Vite dev server. **Every** backend call
(`/api/v1/**`, `/hls/**`, `/events`) is intercepted via `page.route()` and
answered from hand-written fixtures in `browser-tests/helpers.ts` — the Go
server, DB, Redis, and S3 are not exercised. These are frontend smoke tests,
not end-to-end tests. Fixture shapes are typed against the tygo-generated
`$lib/api-types/` file, so drift from the server structs is a compile error,
but runtime behavior downstream of the HTTP boundary is untested.

**Real e2e tests** (`cd e2e && bun run test`): Playwright specs in `e2e/`
that drive the live `docker compose --profile test` stack — real Go server,
Postgres, Redis, MinIO, and three synthetic test cameras. Covers the seams
between layers that nothing else can: JWT round-trip, SSE telemetry delivery,
HLS manifest generation from real segment rows. See `e2e/README.md` for
what is and isn't covered and how to run locally. Takes ~1–2 minutes per
run; gated behind `go` + `ui` + `docker` jobs in CI.

**CI** (`.github/workflows/ci.yml`): Runs `go vet`, `go test`, `go generate ./... (drift check)`,
`bun run check`, `bun run test`, `bun run build`, Docker build, and the
`e2e` job (compose up, Playwright, compose down) on every push/PR. The
`ui/browser-tests/` suite is not run in CI — it's a local smoke test only.

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
- `50000-50200/udp` — WebRTC ICE-lite (server → viewer media)

## Architecture

The server is an HTTP API (Go/chi) with an in-memory WebRTC SFU for low-latency live viewing. Cameras upload MPEG-TS segments directly to S3 via presigned PUT URLs, POST telemetry over HTTP, and maintain a WebSocket to relay raw H.264 frames for live WebRTC viewers. The HLS path (manifests from DB, 302 redirects to S3) is unchanged and serves as the recording backbone, VOD/timeline transport, and automatic fallback when WebRTC is unavailable.

```
Camera (rpicam-vid) ─── raw H.264 ───┬──→ ffmpeg ──┬─→ MPEG-TS segments → S3 (HLS recording)
                                      │             └─→ Opus audio ──┐
                                      └──→ H.264 via WebSocket ──────┤
                                                                     ↓
Server (Go) ← presigned URLs → Browser (hls.js)              pion SFU
     ↓                                                    (H.264 + Opus)
  Postgres (segments, users, cameras, billing)                   ↓
  Redis (telemetry streams, SSE pub/sub, events)    Browser (RTCPeerConnection)
```

Note: when a viewer is watching via WebRTC, the camera uploads video to
both S3 (segments for recording) and the server (WebSocket for live relay).
This doubles upload bandwidth during active viewing but is idle otherwise.

- **Hybrid HLS/WebRTC** -- WebRTC provides sub-second live viewing via pion SFU (ICE-lite, no TURN). HLS always runs as fallback. Viewer shows "LIVE" (WebRTC) or "DELAYED" (HLS fallback). VOD/clip/timeline uses HLS only. Camera sends both H.264 video and Opus audio (32kbps, low-delay) over the WebSocket; ffmpeg encodes Opus alongside AAC from the same ALSA input.
- **On-demand media relay** -- cameras maintain a persistent WebSocket but only send media when a viewer is watching. Server sends `start_stream`/`stop_stream` control messages. **Bandwidth note**: when a viewer is watching live via WebRTC, the camera uploads video twice — once to S3 (HLS segments) and once to the server (WebSocket). This roughly doubles upload bandwidth during active viewing. On cellular links this can degrade HLS upload reliability; the system handles this gracefully (retry with backoff, pending confirms). When no one is watching, bandwidth is identical to before.
- **Ed25519 camera identity** -- each camera generates a permanent ed25519 keypair on first boot (`/var/ghostcam/identity_key` + `identity_key.pub`). Device ID is `SHA-256(public_key)[:16]` hex — deterministic and stable across servers. Cameras authenticate requests by signing `METHOD\nPATH\nTIMESTAMP\nDEVICE_ID` with their private key (`Signature` auth header). Server verifies against the registered public key in `camera_public_keys` table. Switching servers requires only a new provision token — the keypair and device ID are unchanged.
- **QR provisioning** -- on first boot without credentials, the camera scans for a provisioning QR code via `rpicam-still` + `gozxing` (pure Go, no CGO). QR payload carries server URL, token, and optional WiFi creds. Resolution order: CLI/env → flat files → QR scan (5-min timeout). Build-tag gated (`linux && !synthetic`); no-op stub on other platforms.
- **Pi image publishing (webhook-driven)** -- `server/firmware.go` implements `POST /api/v1/webhooks/github`, validates `X-Hub-Signature-256` with `GITHUB_WEBHOOK_SECRET`, pulls `ghostcam-{zero2w,pi4,pi5}-{tag}.img.xz` assets from `release.published` events, uploads them to S3 at `firmware/{version}/ghostcam-{device}.img.xz`, and stores per-device metadata in Redis keys `firmware:images:{device}` (JSON `{version, size_bytes, sha256}`). `GET /api/v1/firmware/images` is the public read path the UI's Get Started onboarding card calls — no manual admin upload step.
- **Camera telemetry over HTTP** -- cameras POST telemetry every 10s, upload segments via presigned PUT URLs
- **Stateless server** -- JWT auth, no sessions table, horizontally scalable
- **S3-native** -- segments served directly from Tigris edge via 302 redirect, no proxy
- **No cleanup daemons** -- retention is enforced by opportunistic prune in the presign handler (DB rows + matching S3 objects, bounded LIMIT 100 per call); there are no hourly session/segment/stale-camera sweep goroutines. Complementary user-initiated deletion runs through `DELETE /api/v1/cameras/:id/footage` (optional `from_ms`/`to_ms`), which the UI loops until the server reports `has_more=false`. The same path is embedded in camera deletion so `DELETE /api/v1/cameras/:id` no longer leaves S3 objects orphaned. We do not use an S3 bucket lifecycle rule because firmware binaries share the bucket and must not be auto-expired.
- **Fail-closed tier handling** -- `billing.GetTier` returns `(Tier, bool)`; unknown tier IDs never fall back to an unlimited tier. `effectiveTier` validates the DB-stored tier string and falls back to free on unknown. Stripe webhooks log loudly and refuse to escalate the user to a paid tier if the price ID is unrecognised.
- **Single-instance required for WebRTC** -- the server holds in-memory state for live WebRTC sessions (camera WebSocket connections, viewer PeerConnections). This pins WebRTC to a single server instance. HLS remains fully stateless and distributable. If horizontal scaling is needed, the path is sticky routing by deviceID: a consistent hash on the device ID in the URL ensures the camera WebSocket and viewer WHEP request hit the same instance. Alternatively, the WebRTC SFU can be extracted into a dedicated media service.

For detailed subsystem documentation see:
- **[docs/usage.md](docs/usage.md)** — Camera setup (flash/deb/binary) and viewer walkthrough: enrolling, playback, clips, billing
- **[docs/api.md](docs/api.md)** — API endpoints, SSE events, camera-server protocol
- **[docs/architecture.md](docs/architecture.md)** — Camera, server, and viewer file-by-file structure
- **[docs/configuration.md](docs/configuration.md)** — Environment variables, config files, billing tiers, retention & cleanup
- **[docs/debugging.md](docs/debugging.md)** — Troubleshooting common issues

## Code Conventions

### Go

- **Error handling**: Return `error` from functions, wrap with `fmt.Errorf("context: %w", err)`.
- **Logging**: `log/slog` with structured fields: `slog.Info("connected", "device_id", id)`.
- **HTTP**: chi router. Handlers are methods on `*App` in the `server` package. JSON responses via `writeJSON()`.
- **Database**: pgx v5 pool, concrete `*db.DB` type (no `Database` interface — tests cover pure functions). Batch inserts via `pgx.Batch`.
- **Concurrency**: `sync.WaitGroup` for goroutine lifecycle, `sync/atomic` for flags, channels for inter-goroutine communication.
- **Build tags**: `//go:build linux && !synthetic` for real sensors (gpsd, /proc, nmcli) and QR scanning (rpicam-still). `//go:build !linux || synthetic` for synthetic sensors and QR no-op stubs. Docker camera target uses `-tags synthetic`. Production Pi builds use real sensors with no synthetic code.

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
| `github.com/makiuchi-d/gozxing` | Pure Go QR code decoder (camera-side provisioning) |
| `golang.org/x/crypto/argon2` | Password hashing (Argon2id) |
| `github.com/pion/webrtc/v4` | WebRTC SFU for low-latency live viewing (ICE-lite, H.264 RTP) |
| `nhooyr.io/websocket` | WebSocket for camera→server live H.264 relay |
| `svelte` (5) | Frontend. Runes: `$state`, `$derived`, `$effect` |
| `tailwindcss` (4) | OKLCH color system, `@import "tailwindcss"` |
| `hls.js` (1) | HLS playback in browser |
| `@ffmpeg/ffmpeg` (0.11.6) | Client-side MP4 assembly via ffmpeg.wasm (lazy-loaded) |
| `bits-ui` (2) | Headless component primitives |
| `leaflet` (1.9) | Map |
| `qrcode` (npm) | Client-side QR SVG generation for enrollment |
