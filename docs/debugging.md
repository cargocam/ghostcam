# Debugging Tips

- **Telemetry API 503**: `GHOSTCAM_REDIS_URL` is unset or empty -- Redis is required for telemetry history and SSE events.
- **Camera not provisioning**: Check the provision token is valid and not expired. Camera POSTs to `POST /api/v1/cameras/provision`. Rate limited to 10/min per IP. Provisioning resolves inputs in order: CLI/env → flat files → QR scan (5-min timeout).
- **Camera unclaimed**: Provision via QR code in the web UI (`GET /api/v1/cameras/enroll/qr`) or API (`POST /api/v1/cameras` to get a provision token). Pre-provision by writing `api_key`, `device_id`, `server_url` files to the camera's data dir.
- **QR scan not starting**: QR scanning requires `rpicam-still` on PATH AND `libzbar0` system library + `pyzbar`/`Pillow` Python bindings (the `[real]` install extra). Synthetic platforms (Docker, dev machines, `GHOSTCAM_SYNTHETIC=1`) skip the QR path entirely. Check logs for "scanning for provisioning QR code" — if absent, the camera logged "rpicam-still not on PATH, skipping QR scan" or "pyzbar/Pillow not installed; install ghostcam[real] for QR support". The scan captures 640x480 YUV420 frames at 2 fps for up to 5 minutes.
- **QR scan not decoding**: Ensure adequate lighting and hold the QR code 15–30 cm from the lens. The decoder (pyzbar / libzbar0) works on the luminance (Y) plane only — high contrast helps. If the QR contains non-JSON or is missing the `s` (server) or `t` (token) fields, the camera silently keeps scanning. Tail the camera with `GHOSTCAM_LOG_LEVEL=DEBUG` to see per-frame decode failures.
- **Capture pipeline crashing**: Pipeline auto-restarts with exponential backoff (1s to 30s cap). Crash counter with 5-minute stability threshold. Check for ffmpeg availability and rpicam-vid on real hardware.
- **Segment filename collisions**: ffmpeg uses `-segment_start_number` based on existing `.ts` file count to avoid overwriting segments on camera restart.
- **GPS not working**: On Linux, the camera connects to gpsd on `localhost:2947`. Ensure gpsd is running and the SIM7600G-H modem is connected. Falls back to synthetic GPS in Docker/dev (deterministic from device serial hash).
- **Motion detection**: Uses ffprobe P-frame analysis to detect motion, falls back to file size heuristic (1.5x rolling average of last 10 segments). The `has_motion` column in the `segments` table tracks this per-segment.
- **Storage cap**: When a user exceeds their tier's storage limit, the presign handler uses Redis `INCRBY` for atomic reservation (prevents TOCTOU race), returns `storage_capped: true`, and emits a deduplicated `storage_capped` SSE event (5 min cooldown per device via Redis `SETNX`). Uploads are paused until storage is freed or the user upgrades. **Live WebRTC streaming is not gated by the storage cap** — the WHEP and `CameraLiveWS` paths never touch the presign handler, so cameras stay watchable in real time while recording is paused. The UI surfaces this state via a persistent banner (`StorageCapBanner.svelte`) driven by `billingStore.isStorageCapped` and `storagePercent`; the transport store refreshes billing usage on every incoming `storage_capped` SSE event so the banner reflects server state without the user needing to re-open settings.
- **Local storage eviction**: The camera evicts oldest segments when local storage exceeds 4 GB (default). Configure via `GHOSTCAM_LOCAL_STORAGE_CAP_MB` environment variable or `local_storage_cap_mb` in the camera config file.
- **Upload failures**: Upload retry queue with 3 retries and exponential backoff (2s, 4s, 8s). After max retries, segment stays on disk (not deleted). `storageCapped` uses `atomic.Bool` to avoid data races.
- **Audit trail**: Security-relevant events (`auth_success`, `enrollment_started`, `camera_provisioned`, `camera_unregistered`) are emitted as structured `slog.Info("audit", "event_type", …)` records. There is no dedicated DB table — grep the server logs.
- **Billing always on**: Every user defaults to the "free" tier (5 GB, 1 camera). `effectiveTier()` derives tier from Stripe subscription state and validates the stored tier string against `billing.Tiers`; unknown strings fall back to free (fail-closed). When Stripe is not configured (dev), returns "enterprise" (unlimited) so local testing works without payment infrastructure. Set `STRIPE_SECRET_KEY` in `.env` to enable real tier enforcement.
- **Camera limit 402**: When a user hits their tier's camera limit, `POST /api/v1/cameras` returns HTTP 402 with `{ error: "camera_limit_reached" }`.
- **Camera limit on downgrade**: On tier downgrade, excess cameras are soft-blocked: presign returns `storage_capped: true` for cameras beyond the limit (oldest N by `enrolled_at` remain active). Read access (HLS, telemetry) is preserved for all cameras. No cameras are deleted.
- **Failed login logging**: Login failures are logged with email + IP address (via `X-Forwarded-For` or `RemoteAddr`).
- **HLS segment expiry**: Manifests use relative `.ts` paths. Each segment request re-presigns on the fly and returns 302 to S3. No presigned URLs in manifests means no mid-stream expiry.
- **Billing webhooks**: Stripe webhooks keep subscription state in sync. In production, set `STRIPE_WEBHOOK_SECRET` to the real signing secret.
- **Firmware OTA**: Admin uploads firmware via `POST /api/v1/admin/firmware` (stored in Tigris, version published via Redis). Cameras check `GET /api/v1/firmware/latest` on startup and auto-update via staged binary + systemd `ExecStartPre` swap. Firmware SHA256 verification (server stores hash, camera verifies).
- **Pre-encoded test loop**: Place `test-loop.mp4` in the camera's data dir for low-CPU test mode (~5% vs 49% with testsrc2 encoding). The camera uses `ffmpeg -stream_loop` to segment it continuously.
- **Unenroll script**: `pi.sh unenroll` clears credential files (`api_key`, `device_id`, `server_url`) from the camera's data dir.
- **Server memory / RSS investigation** (`GH #56`): The server can expose Go's `net/http/pprof` handlers on a separate loopback-only listener. Off by default. Set `GHOSTCAM_PPROF_ADDR=127.0.0.1:6060` on the Fly app (`fly secrets set GHOSTCAM_PPROF_ADDR=127.0.0.1:6060`) and the server starts a second listener that serves `/debug/pprof/*`. **Loopback-only by design** — the handlers are unauthenticated and dump goroutine state, allocations, and live memory. Reach the endpoint with:
  ```bash
  fly ssh console -a ghostcam
  curl -s http://127.0.0.1:6060/debug/pprof/heap > /tmp/heap.pprof
  # then on your laptop:
  fly ssh sftp shell -a ghostcam   # `get /tmp/heap.pprof`
  go tool pprof -http=:8080 heap.pprof
  ```
  Useful profiles: `heap` (live allocations), `goroutine` (stuck goroutines / leaks), `allocs` (cumulative — pair with `?seconds=30`), `profile` (30s CPU sample). To grab everything in one shot:
  ```bash
  curl -s http://127.0.0.1:6060/debug/pprof/heap > heap.pprof
  curl -s http://127.0.0.1:6060/debug/pprof/goroutine > goroutine.pprof
  curl -s "http://127.0.0.1:6060/debug/pprof/profile?seconds=30" > cpu.pprof
  ```
  Locally: bring the test stack up (`docker compose --profile test up -d`), `docker compose exec ghostcam-server sh`, then curl as above. Unset the env var (or `fly secrets unset GHOSTCAM_PPROF_ADDR`) to disable.
