# e2e — real end-to-end tests

Playwright specs that drive the **live** docker-compose stack: Go server,
Postgres, Redis, MinIO, test cameras. Unlike `ui/browser-tests/` (which
mocks every backend route) these tests prove the seams between layers
actually work.

## What these cover that nothing else does

- Vite dev server correctly proxies `/api/*` to the Go server
- Admin login seeded by `db.Initialize` works end-to-end
- Camera provisioning handshake (POST /provision → api_key)
- Camera → server telemetry poll → Redis XADD → SSE XREAD → UI flip
- Camera → server presign → MinIO upload → confirm → DB row
- GetLiveManifest assembles a spec-compliant HLS manifest from real
  segment rows
- JWT cookie + chi middleware + pgx connection pool all wired together

## What they do NOT cover

- Feature breadth (motion detection heuristics, clip export, UI
  interactions beyond login). Those are unit tests or `browser-tests/`.
- Stripe billing (the stripe-webhooks container is best-effort in dev).
- Real hardware camera behavior (rpicam-vid, gpsd). Tests run against
  `--tags synthetic` camera binaries using `docker/test-loop.mp4`.

## Running locally

```bash
# Set your LAN IP so MinIO presigned URLs work from both the containers
# and your browser.
export GHOSTCAM_PUBLIC_IP=$(ipconfig getifaddr en0)   # macOS
# export GHOSTCAM_PUBLIC_IP=$(hostname -I | awk '{print $1}')   # Linux

# Keep Stripe empty so the admin's free tier is replaced by the dev-mode
# "enterprise" fallback in effectiveTier — otherwise only the first
# test camera provisions and the rest crashloop at the 1-camera limit.
unset STRIPE_SECRET_KEY

# Bring the stack up with the 3 synthetic test cameras.
docker compose --profile test up -d

# Install e2e deps + Chromium (first time only).
cd e2e
bun install
bunx playwright install --with-deps chromium

# Run.
bun run test

# Cleanup.
cd ..
docker compose down -v
```

The first run can take ~30s after `up -d` for the stack to be ready and
another ~15s before the first segment appears in the HLS manifest. The
spec waits with generous timeouts.

## Running in CI

`.github/workflows/ci.yml` has an `e2e` job that runs the same sequence
on every PR. It's gated on `go`, `ui`, and `docker` jobs succeeding so
flaky e2e failures don't block unrelated work. On failure, compose
logs and Playwright traces are uploaded as artifacts.

## Troubleshooting

- **`server did not become ready within 60000ms`** — the compose stack
  didn't start. Run `docker compose ps` to see which container is
  unhealthy, then `docker compose logs server` (or postgres / redis /
  minio) to diagnose.

- **`no camera cards visible`** — cameras didn't provision. Most likely
  `STRIPE_SECRET_KEY` is non-empty, which caps the admin at 1 camera.
  `unset STRIPE_SECRET_KEY` and bring the stack back up.

- **`live.m3u8 has no #EXTINF`** — the camera isn't uploading. Check
  `docker compose logs camera-1` for ffmpeg errors, and verify
  `GHOSTCAM_PUBLIC_IP` is set correctly so presigned URLs point at a
  reachable MinIO.

- **Playwright can't install Chromium** — corporate firewall blocking
  `cdn.playwright.dev`. Set `PLAYWRIGHT_DOWNLOAD_HOST` to an internal
  mirror, or run on a network that allows the CDN.
