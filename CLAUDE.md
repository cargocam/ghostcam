# CLAUDE.md — Ghostcam Development Guide

## Documentation Policy

When making changes to the codebase, **always update the relevant READMEs,
`docs/`, and this file** to reflect them. Detailed reference docs live in
`docs/` — keep them in sync with the code.

## What is this project?

Ghostcam is a self-hosted camera surveillance system in Go. Cameras capture
H.264 + AAC via `rpicam-vid | ffmpeg`, upload MPEG-TS segments to S3 (Tigris)
via presigned URLs, and POST telemetry over HTTP. The server generates HLS
manifests on the fly, serves segments via 302 redirect to S3, runs a pion
WebRTC SFU for sub-second live viewing, and exposes a REST + SSE API consumed
by a Svelte 5 viewer.

See the topology diagram in [README.md](README.md#architecture);
[docs/architecture.md](docs/architecture.md) has the file-by-file breakdown.

### Architectural notes

- **Hybrid HLS + WebRTC.** Recording is always HLS: segments on S3, manifests
  built from DB rows, `.ts` requests 302-redirect to re-presigned S3 URLs.
  Live viewing uses a pion SFU (ICE-lite, no TURN); HLS is the fallback. The
  viewer shows "LIVE" when WebRTC is up and "DELAYED" when it's on HLS.
  VOD/clip/timeline is HLS only.
- **On-demand relay.** Cameras keep a persistent WebSocket but only send
  H.264 + Opus frames between server `start_stream` / `stop_stream` control
  messages. When a viewer is active, upload bandwidth roughly doubles
  (S3 segments + WebSocket relay).
- **Ed25519 identity.** Each camera mints a permanent keypair on first boot
  (`/var/ghostcam/identity_key[.pub]`). Device ID = `SHA-256(pubkey)[:16]`.
  Requests are signed per-call
  (`Signature` header over `METHOD\nPATH\nTIMESTAMP\nDEVICE_ID`) and verified
  against `camera_public_keys`. Switching servers only requires a new
  provision token — the keypair and device ID persist.
- **QR provisioning.** On first boot without credentials, the camera scans
  for a QR via `rpicam-still` + `gozxing` (pure Go, no CGO). Resolution
  order: CLI/env → flat files → QR scan (5-min timeout). Build-tag gated
  (`linux && !synthetic`); no-op stub elsewhere.
- **Pi image publishing.** `server/firmware.go` handles
  `POST /api/v1/webhooks/github`, validates `X-Hub-Signature-256` with
  `GITHUB_WEBHOOK_SECRET`, and mirrors `release.published`'s
  `ghostcam-{zero2w,pi4,pi5}-{tag}.img.xz` assets into S3 at
  `firmware/{version}/…`. Metadata lives in Redis keys
  `firmware:images:{device}`. The UI reads `GET /api/v1/firmware/images`.
- **Stateless HTTP path.** JWT auth, no sessions table. The WebRTC SFU holds
  in-memory per-device state, pinning live viewing to one instance; HLS is
  fully stateless. If scaled horizontally, sticky-route by deviceID or split
  the SFU into its own service.
- **No cleanup daemons.** Retention is enforced opportunistically in the
  presign handler — DB rows and matching S3 objects are deleted together
  (LIMIT 100/call). User-initiated deletion runs through
  `DELETE /api/v1/cameras/:id/footage` (loops until `has_more=false`) and is
  embedded in `DELETE /api/v1/cameras/:id`. We don't use S3 lifecycle rules
  because firmware binaries share the bucket.
- **Fail-closed billing.** `billing.GetTier` returns `(Tier, bool)` — unknown
  tier IDs never grant unlimited resources. `effectiveTier` falls back to
  free on unknown DB values, and Stripe webhooks refuse to escalate on
  unrecognised price IDs.

## Repository layout

```
ghostcam/
├── common/          Shared Go types: camera<->server contract (telemetry, presign, provisioning)
├── camera/          Camera binary (package main): capture, upload, telemetry,
│                    provisioning (CLI/env → files → QR), gpsd, firmware
├── server/          Server binary (package main): chi router, handlers as methods on *App
│   ├── apitypes/    Viewer<->server request/response + SSE payload types
│   │                (tygo source of truth along with common/)
│   ├── auth/        Argon2id, JWT, HMAC token hashing
│   ├── mailer/      Resend transactional email (verify, reset, OTP, change-email)
│   ├── billing/     Tier definitions and storage limits
│   ├── db/          Postgres (pgx), migrations, record types — concrete *DB, no interface
│   ├── redis/       Telemetry streams (XADD/XREAD), pub/sub for SSE, event storage
│   ├── s3/          S3/Tigris presigned URLs, Upload, Delete
│   ├── triage/      Anthropic Messages classifier for inbound support email (no-op without ANTHROPIC_API_KEY)
│   └── linear/      Linear GraphQL issueCreate client (no-op without LINEAR_API_KEY)
│                    Inbound webhook handler lives in server/support.go.
├── tygo.yaml        common/ + server/apitypes/ → ui/src/lib/api-types/ (driven by go generate)
├── ui/              Svelte 5 SPA — HLS playback, timeline scrubber, GPS map
│   └── src/lib/api-types/   Generated TypeScript types — DO NOT EDIT
├── e2e/             Playwright specs that drive the live docker-compose stack
├── pi/              systemd, GPS, NetworkManager
│   └── image/       rpi-image-gen build for flashable .img
├── scripts/         pi.sh — camera manager CLI
├── docs/            Detailed reference: API, architecture, configuration, debugging
├── Dockerfile       Multi-stage: server, camera (synthetic), camera-prod (real sensors)
└── docker-compose.yml  Server + UI + MinIO + Stripe listener + 3 test cameras (--profile test)
```

## Build & run

Every service runs through docker-compose. Never run server, cameras, or UI
natively. Vite serves the UI with HMR in dev; the Go server serves the built
static files directly in prod.

```bash
# Build
go build -o ghostcam-server ./server
go build -o ghostcam-camera ./camera
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ghostcam-camera ./camera   # for Pi

# Regenerate TS types from Go source — run after editing common/ or server/apitypes/
go generate ./...

# Set your LAN IP once
echo "GHOSTCAM_PUBLIC_IP=$(ipconfig getifaddr en0)" > .env
docker compose build
```

Two workflows:

```bash
# Server/UI dev with synthetic cameras
docker compose --profile test up -d
# http://localhost:5173  —  admin@ghostcam.dev / dev-password

# Real Pi hardware — server + UI only, pi.sh handles cross-compile + deploy
docker compose up -d
./scripts/pi.sh deploy
```

Both can run simultaneously — synthetic and real cameras talk to the same
server. Defaults come from `.pi.env` (gitignored); positional args override.
Clean restart: `docker compose down -v && docker compose up -d`.

| Subcommand | Args | Purpose |
|------------|------|---------|
| `setup`    | `[HOST] [USER] [PASS]`        | First-time Pi provisioning |
| `deploy`   | `[HOST] [USER] [PASS]`        | Cross-compile + deploy + tail logs (primary dev loop) |
| `logs`     | `[HOST] [USER] [PASS]`        | Stream camera logs |
| `status`   | `[HOST] [USER] [PASS]`        | Health check |
| `restart`  | `[HOST] [USER] [PASS]`        | Restart camera service |
| `ssh`      | `[HOST] [USER] [PASS]`        | Interactive SSH |
| `unenroll` | `[HOST] [USER] [PASS]`        | Reset camera enrollment state |
| `wifi-off` | `[SECS] [HOST] [USER] [PASS]` | Drop WiFi for N seconds (cellular failover test) — **`SECS` comes first** |

## API type generation

The UI's sole source of truth for wire types is `server/apitypes/apitypes.go`
(viewer<->server) and `common/types.go` + `common/telemetry.go`
(camera<->server). `tygo` writes matching TS interfaces to
`ui/src/lib/api-types/`, and every consumer — including `browser-tests/`
fixtures — imports from `$lib/api-types`. Drift is a compile error.

To change a wire shape:

1. Edit the Go struct.
2. Run `go generate ./...`.
3. Commit both the Go change and the regenerated TS files.

CI enforces this via `git diff --exit-code` against the regenerated output.

## Testing

- **Go** (`go test ./...`) — table-driven tests for pure functions. Covers
  `effectiveTier`, `epochMsToISO8601`, `TestCameraLimitAllowed`,
  `billing.GetTier`/`StorageLimitBytes`, camera motion detector, MPEG-TS
  sync-byte validation, pending-confirm persistence, config helpers, and the
  H.264 NAL parser.
- **UI unit** (`cd ui && bun run test`) — vitest in `ui/src/lib/__tests__/`.
  Coverage merge, alert dedup, time formatting, storage-cap banner
  thresholds.
- **UI browser** (`cd ui && bun run test:browser`) — Playwright in
  `ui/browser-tests/` against the Vite dev server. **Every** backend call is
  stubbed via `page.route()` from typed fixtures in `helpers.ts`. Frontend
  smoke only; not run in CI.
- **End-to-end** (`cd e2e && bun run test`) — Playwright against the live
  `docker compose --profile test` stack. Exercises JWT, SSE delivery, HLS
  manifest generation from real segment rows. See `e2e/README.md`. Takes
  1–2 min; gated behind `go`/`ui`/`docker` in CI.

**CI** (`.github/workflows/ci.yml`): `go vet`, `go test`, tygo drift check,
`bun run check`, `bun run test`, `bun run build`, Docker build, then `e2e`.

## CI / Release

- **`.github/workflows/ci.yml`** on push/PR to main:
  - `go`: vet, test, drift check
  - `ui`: `bun install --frozen-lockfile`, `check`, `build`
  - `docker`: server + camera targets with BuildKit cache
  - `e2e`: compose up → Playwright → compose down
- **`.github/workflows/release.yml`** on `v*` tags:
  - `build-camera`: aarch64 + x86_64 binaries
  - `build-camera-deb`: aarch64 `.deb` (depends on ffmpeg, ca-certificates)
  - `build-pi-image`: flashable `.img` for zero2w, pi4, pi5 via `rpi-image-gen`
  - `build-docker`: server image → GHCR
  - `release`: attaches binaries, `.deb`, `.img.xz`, and `checksums.txt`

## Key ports

- `3000/tcp` — HTTP API + static viewer
- `5173/tcp` — Vite dev (proxies `/api`, `/hls`, `/events` → :3000)
- `50000-50200/udp` — WebRTC ICE-lite (server → viewer media)

## Further reading

- **[docs/usage.md](docs/usage.md)** — camera setup (flash/deb/binary) and viewer walkthrough
- **[docs/api.md](docs/api.md)** — endpoints, SSE events, camera-server protocol
- **[docs/architecture.md](docs/architecture.md)** — file-by-file structure
- **[docs/configuration.md](docs/configuration.md)** — env vars, billing tiers, retention
- **[docs/debugging.md](docs/debugging.md)** — troubleshooting

## Code conventions

### Go

- **Errors**: return `error`, wrap with `fmt.Errorf("context: %w", err)`.
- **Logging**: `log/slog` with structured fields —
  `slog.Info("connected", "device_id", id)`.
- **HTTP**: chi router; handlers are methods on `*App`. JSON responses via
  `writeJSON()`.
- **DB**: pgx v5 pool, concrete `*db.DB` (no `Database` interface — tests
  cover pure functions). Batch inserts via `pgx.Batch`.
- **Concurrency**: `sync.WaitGroup` for goroutine lifecycle, `sync/atomic`
  for flags, channels for inter-goroutine communication.
- **Build tags**: `//go:build linux && !synthetic` for real sensors (gpsd,
  `/proc`, nmcli) and QR scanning (`rpicam-still`); `//go:build !linux ||
  synthetic` for synthetic sensors and stub QR. The Docker camera target
  uses `-tags synthetic`; prod Pi builds do not.

### Svelte / TypeScript

- **Svelte 5 runes only**: `$state`, `$derived`, `$effect`, `$props()`. No
  legacy `$:`.
- **Stores**: exported object literals with `$state` fields — not classes.
- **Styling**: Tailwind CSS 4 utilities, OKLCH tokens in `app.css`, `cn()`
  for merging.
- **Components**: bits-ui primitives in `components/ui/`; domain components
  live next to their views.
- **localStorage**: keys prefixed with `ghostcam-`.

## Key dependencies

| Package | Notes |
|---------|-------|
| `github.com/go-chi/chi/v5` | HTTP router |
| `github.com/jackc/pgx/v5` | Postgres driver (connection pool) |
| `github.com/redis/go-redis/v9` | Redis client (Streams, pub/sub) |
| `github.com/aws/aws-sdk-go-v2` | S3/Tigris presigned URLs |
| `github.com/pion/webrtc/v4` | WebRTC SFU (ICE-lite, H.264 RTP) |
| `nhooyr.io/websocket` | Camera→server live H.264/Opus relay |
| `github.com/makiuchi-d/gozxing` | Pure Go QR decoder (camera provisioning) |
| `golang.org/x/crypto/argon2` | Password hashing (Argon2id) |
| `github.com/BurntSushi/toml` | Config file parsing |
| `github.com/google/uuid` | Segment IDs |
| `svelte` (5) | Frontend — runes only |
| `tailwindcss` (4) | OKLCH color system, `@import "tailwindcss"` |
| `hls.js` (1) | HLS playback |
| `@ffmpeg/ffmpeg` (0.11.6) | Client-side MP4 assembly (lazy-loaded) |
| `bits-ui` (2) | Headless component primitives |
| `leaflet` (1.9) | Map |
| `qrcode` | Client-side QR SVG for enrollment |
