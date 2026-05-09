# Architecture Details

## Shared Types (`common/`)

```
types.go       PresignRequest, PresignResponse, PresignedUrl, UploadedSegment, ProvisionRequest/Response
               CameraCommand — server→camera commands delivered via telemetry poll response
               (set_resolution, set_recording_mode, reboot, unregister, network_config, remove_network)
telemetry.go   TelemetryDatagram — JSON payload with optional fields (CPU, temp, mem, GPS, wifi signal, uptime)
```

## Camera Structure

The camera daemon is being ported from Go to Python. Both implementations
exist on disk during the port; `docker compose --profile test` and
`./scripts/pi.sh deploy` use the Python build. The Go camera under
`camera/` stays buildable until the cutover commit deletes it.

### Python camera (`ghostcam-py/`) — primary

```
ghostcam-py/
  pyproject.toml          Hatchling wheel build, console_scripts entry: `ghostcam-camera`
  ghostcam/
    main.py               Entrypoint: load config, signal handling, asyncio.TaskGroup with
                          5 tasks (live_ws, capture supervisor, telemetry, watcher, upload),
                          15s graceful drain on SIGINT/SIGTERM. Mirrors camera/main.go's
                          goroutine structure as cancellable asyncio tasks.
    config.py             CameraConfig + tomllib + argparse + env layering — same
                          /boot/ghostcam.conf → {dataDir}/camera.toml → env → flags precedence
                          as the Go camera, including the persisted runtime overrides at
                          {dataDir}/recording_mode and {dataDir}/resolution.
    identity.py           PyNaCl ed25519 keypair with hex-on-disk seed at {dataDir}/identity_key.
                          PyNaCl produces byte-identical signatures to Go's crypto/ed25519
                          (verified by tools/sigverify cross-language harness).
    credentials.py        Flat-file persistence; permanent identity, replaceable server_url.
    signing.py            Authorization: Signature device_id=<hex>,ts=<int>,sig=<b64>
                          builder. Unix SECONDS in the signed payload (telemetry uses ms).
    client.py             httpx.AsyncClient: post_telemetry, request_presigned_urls,
                          upload_segment (PUT to S3 with Content-Type video/mp2t), provision.
    capture.py            asyncio.create_subprocess_exec orchestration of rpicam-vid + ffmpeg.
                          Audio side-channel uses `pipe:{wfd}` with pass_fds — NOT Go's fixed
                          fd 3 layout, which silently breaks under Python's child interpreter
                          (verified during planning Spike 2).
    ogg_reader.py         OGG page parser → Opus packets, async-friendly.
    watcher.py            Polls segment_dir every 2s for finished .ts files, validates the
                          0x47 sync byte, runs MotionDetector, pushes onto upload queue.
                          Seeds known set from pending_confirms.json on startup.
    upload.py             asyncio upload loop with PendingConfirms (atomic JSON store),
                          retries (2s/4s/8s backoff), 4xx-clears-URL-cache, storage-cap,
                          server-unreachable flag observed by capture supervisor.
    motion.py             ffprobe P-frame size analysis with file-size fallback —
                          same 1.5x-rolling-avg threshold as the Go camera.
    live_relay.py         H.264 Annex B start-code scanner (pure-Python bytes.find;
                          ~221 MB/s on Pi Zero 2W per planning Spike 1, ~883x margin
                          over the production rate). asyncio.Queue(maxsize=120) ring
                          with drop-oldest semantics. _find_nal_boundaries is the
                          isolated swap point for a future Rust+pyo3 native module.
    live_ws.py            websockets client; on-connect "ready" JSON message, then
                          binary frames `[ts:4 BE][flags:1][payload]` — byte-identical
                          to camera/live_ws.go::sendFrame. start_stream/stop_stream
                          control messages flip the streaming gate.
    telemetry_poll.py     10s loop, 30s after 3 fails, 60s after 5; writes boot_ok
                          marker after first successful POST.
    commands.py           reboot, unregister, set_recording_mode, set_resolution,
                          network_config, update_firmware, remove_network — most
                          os._exit(0) for systemd to restart with new state.
    provisioning.py       CLI/env → flat files → QR scan resolution; QR-supplied WiFi
                          credentials trigger ensure_wifi + wait_for_route.
    firmware.py           Self-update: GET /api/v1/firmware/latest, sha256-verified
                          download to {dataDir}/staged-update.deb, exit for systemd.
    platform/             Replaces Go's build tags. selected at import time:
      __init__.py           GHOSTCAM_SYNTHETIC=1 → synthetic; Linux+/proc → linux; else synthetic
      synthetic.py          Deterministic Seattle-orbit GPS, fixed CPU/mem/temp — Docker/CI
      linux.py              /proc/stat, /proc/meminfo, /sys/class/thermal, /proc/net/wireless,
                            gpsd, nmcli, rpicam-still + pyzbar QR
    wire/                 Generated pydantic v2 models — DO NOT EDIT.
                          Source: common/types.go + common/telemetry.go via tools/pydanticgen.
  tests/                  pytest suite (71 tests):
                          * test_wire_format.py + test_signing_roundtrip.py — every
                            must-not-drift wire item has a fixture; signing parity
                            is enforced by Python signs / Go verifies (and vice versa)
                            via tools/sigverify, byte-identical across the boundary.
                          * test_live_relay.py, test_ogg_reader.py, test_motion.py,
                            test_config.py, test_upload.py, test_watcher.py,
                            test_live_ws.py, test_capture.py, test_platform.py.
```

### Go camera (`camera/`) — DEPRECATED, removed by the cutover commit

```
camera/            (package main — binary builds from this directory)
  main.go          Entrypoint: config, signal handling, goroutine orchestration (WaitGroup),
                   capture crash recovery with exponential backoff (1s→30s) and 5-minute stability threshold,
                   graceful shutdown (WaitGroup drain, 15s timeout). Pure orchestration — all logic in the
                   other camera/ files.
  config.go        CameraConfig + cameraConfigFile, layered TOML/env/CLI resolution
                   RecordingMode ("constant"/"motion"/"never") — runtime override via {dataDir}/recording_mode.
                   "never" (streaming-only) is the default for fresh data dirs and new DB rows: ffmpeg
                   skips the MPEG-TS segment sink and main.go skips the segment watcher + upload loop,
                   so nothing is written to disk or uploaded. The live WebSocket relay is unaffected.
                   LocalStorageCapBytes — configurable via GHOSTCAM_LOCAL_STORAGE_CAP_MB (default 4096 MB)
                   Resolution runtime override via {dataDir}/resolution
                   Video profiles: zero2w/480p, pi4/720p, pi5/1080p
  capture.go       Capture pipeline: rpicam-vid | ffmpeg → MPEG-TS segments (6s each)
                   Test mode: ffmpeg testsrc2 + sine audio, or pre-encoded test-loop.mp4 (~5% CPU vs 49%)
                   Uses -segment_start_number to avoid filename collisions on restart
                   ffmpeg cleanup: SIGTERM to process group, then SIGKILL after 5s
  watcher.go       NewSegment type, motionDetector (ffprobe P-frame analysis, falls back to file size heuristic)
                   RunSegmentWatcher: polls every 2s, skips 0-byte and still-being-written files
                   Backpressure: 5s blocking send to segment channel (drops if full)
                   EnforceLocalStorageCap: evicts oldest .ts files when over cap
  upload.go        RunUploadLoop: consumes segments from channel, uploads via presigned PUT URLs
                   Retry queue: 3 retries with exponential backoff (2s, 4s, 8s)
                   storageCapped: atomic.Bool — pauses uploads when server indicates storage full
                   Pending confirmations persisted to {dataDir}/pending_confirms.json so a crash
                   or restart between S3 PUT and the confirming presign request does not orphan
                   uploaded S3 objects — loaded on startup, cleared after server accepts confirms
                   Graceful shutdown: flushes pending confirmations with 5s timeout
  client.go        HTTP client for server API (telemetry POST, presign, provision, S3 upload)
  credentials.go   LoadCredentials / SaveCredentials — flat files (api_key, device_id, server_url) with 0600 permissions
  provisioning.go  Token-based provisioning via POST /api/v1/cameras/provision
                   Resolution order: CLI/env → flat files → QR scan
                   Supports --provision-token CLI flag / GHOSTCAM_PROVISION_TOKEN env var for headless provisioning
  qr_linux.go      QR code scanning via rpicam-still (YUV420 frames) + gozxing (pure Go ZXing)
                   5-minute timeout, process group cleanup, graceful fallback if rpicam-still missing
                   Build tag: //go:build linux && !synthetic
  qr_other.go      No-op QR stub for non-Linux/synthetic platforms
                   Build tag: //go:build !linux || synthetic
  commands.go      HandleCommand: processes server-issued commands (reboot, unregister, set_resolution, etc.)
  telemetry_poll.go RunTelemetryPoll: 10s poll loop with backoff, processes piggy-backed commands
  motion.go        ffprobe P-frame analysis with file-size fallback for motion detection
  firmware.go      CheckFirmwareUpdate: checks GET /api/v1/firmware/latest on startup, auto-updates
  sensors_linux.go ReadTelemetry: CPU (/proc/stat), memory (/proc/meminfo), temp (/sys/class/thermal),
                   uptime (/proc/uptime), WiFi signal (/proc/net/wireless), GPS (gpsd)
                   Build tag: //go:build linux && !synthetic
  sensors_other.go Synthetic telemetry (GPS, CPU, etc.) for dev/Docker
                   Build tag: //go:build !linux || synthetic
  sensors_common.go  InjectSyntheticGPS: deterministic GPS from device serial hash (for dev/Docker)
  gpsd.go          (Linux) Real gpsd integration: connects to localhost:2947, enables JSON watch,
                   reads TPV reports for lat/lon/alt/fix. Used by SIM7600G-H modem on Pi.
  gpsd_other.go    (non-Linux) No-op gpsd stub
  network_linux.go EnsureWifi (nmcli), WaitForRoute (/proc/net/route polling)
  network_other.go No-op stubs for non-Linux
```

Runtime override files (`{dataDir}/resolution`, `{dataDir}/recording_mode`) are written when the server sends `set_resolution` or `set_recording_mode` commands via the telemetry poll response. The camera reads these on startup and on pipeline restart, allowing remote configuration changes to survive reboots.

## Server Structure

```
server/            (package main — binary builds from this directory)
  main.go         Entrypoint: config load, DB connect, Redis/S3 init, App wiring,
                  chi router construction, HTTP server with timeouts + graceful
                  shutdown. No background cleanup goroutines.
                  HTTP timeouts: Read 30s, Write 60s, Idle 120s
                  Graceful shutdown with 10s timeout
  config.go       ServerConfig + serverConfigFile, layered TOML/env resolution
                  PublicURL for QR codes and CORS origin, retentionDays() helper,
                  secureCookies() derived from the PublicURL scheme
  middleware.go   Context key helpers, viewerAuth (JWT cookie + Bearer API token),
                  cameraAuth (Bearer API key), adminAuth (viewerAuth + DB lookup
                  against the admins table — admin status is not in the JWT)
  ratelimit.go    Per-IP token bucket rate limiter with opportunistic eviction
                  (no dedicated cleanup goroutine)

  apitypes/       Viewer<->server HTTP request/response types and SSE payload types.
                  Types only — no behavior. Read by tygo to generate
                  ui/src/lib/api-types/. Editing a struct here is automatically
                  reflected in the UI types on the next `make generate-types` run;
                  CI's `make check-types` step hard-fails any PR whose generated
                  files are stale.

  auth_handlers.go  Login (with DummyVerify timing equalization), Logout, Register (403),
                    ChangePassword. setAuthCookie uses Config.secureCookies().
  cameras.go        ListCameras / Enroll / GetCamera / UpdateCamera / DeleteCamera.
                    Enroll enforces tier camera limit (402 on exceed), UpdateCamera
                    enqueues commands for resolution/recording_mode changes.
                    DeleteCamera invokes purgeAllFootageForDelete (footage.go)
                    synchronously before dropping the cameras row, so the S3
                    segment objects are reaped instead of orphaned.
  footage.go        DeleteFootage (DELETE /api/v1/cameras/:id/footage with
                    optional from_ms/to_ms), purgeDeviceFootage loop helper
                    (bounded batches of DeleteSegmentsRange + S3.Delete, signals
                    has_more so the UI can page through large purges), and
                    purgeAllFootageForDelete used by both user and admin camera
                    delete paths. Shares DB / S3 plumbing with presign.go's
                    retention prune.
  presign.go        effectiveTier() (fail-closed — unknown tier strings fall
                    back to free, never escalate to a paid tier), resolveTier()
                    (panics on unknown — fed only by already-validated IDs), and
                    the Presign handler. After confirming uploads, opportunistically
                    prunes expired segment rows **and their S3 objects** for this
                    device (PruneSegments LIMIT 100) so there is no retention
                    sweeper goroutine. Storage counter cached in Redis (INCRBY/DECRBY).
  hls.go            GetLiveManifest (90s sliding window, no ENDLIST),
                    GetVodManifest (max 24h range with ENDLIST),
                    GetSegment (302 redirect to S3), GetInit, GetCoverage.
                    All reads clamp to the retention window.
  telemetry.go      PostTelemetry (camera telemetry POST),
                    GetTelemetryLatest, GetTelemetryRange
  clips.go          PrepareClip (presigned segment URLs), ExportTelemetry (CSV/JSON)
  events.go         ListEvents, GetUnreadCount, MarkEventRead, MarkAllEventsRead, DismissEvent
  sse.go            SSE via Redis XREAD + pub/sub, write deadline disabled
  billing.go        GetSubscription, ListTiers, CreateCheckout, CreatePortal, GetUsage,
                    StripeWebhook (idempotency via stripe_events table)
  admin.go          FirmwareLatest (public), FirmwareUpload (admin), GithubWebhook
  support.go        ResendInboundWebhook: Svix-signed `POST /api/v1/webhooks/resend`
                    ingest for customer support email. Verifies signature, dedupes
                    on svix-id via support_tickets INSERT, then hands off to an
                    async goroutine that classifies the email with Claude (optional)
                    and creates a Linear issue. Same shape as GithubWebhook:
                    verify-then-async + bounded concurrency counter
                    (triageInFlight atomic.Int32, cap 16).
  qr.go             EnrollmentQR — returns JSON {payload, token, expires_at}
  provision.go      Provision — camera claims a one-time token and receives an API key.
                    ClaimCommands is atomic DELETE ... RETURNING so commands do not accumulate.
  tokens.go         ListTokens, CreateToken, RevokeToken
  health.go         Healthz (always 200), Readyz (DB ping)

  auth/           Argon2id password hashing, JWT sign/verify, HMAC token hashing, random password generation
  billing/        Tier definitions: Free (5 GB / 1 camera), Starter (50 GB / 4 cameras),
                  Pro (500 GB / 16 cameras), Enterprise (unlimited)
  db/             PostgreSQL via pgx v5 — connection pool, migrations, record types,
                  concrete *DB type (no Database interface; tests cover pure functions)
  redis/          Telemetry write (XADD) and query (XREAD), per-user pub/sub channels
                  for motion, storage_capped, and coverage events. Redis-cached storage
                  counter storage_bytes:{user_id} (5-min TTL). Event storage (events.go).
  s3/             S3/Tigris client: presigned GET/PUT, Upload (for firmware),
                  Delete (used by the opportunistic prune path in presign.go).
                  No bucket-level lifecycle rules — firmware lives in the same
                  bucket and must not be auto-expired.
  triage/         Support-email classifier wrapping the Anthropic Messages API.
                  Returns a Linear-shaped Result (Title/Description/Priority/
                  Category). Disabled (ErrDisabled) when ANTHROPIC_API_KEY is
                  unset; callers fall back to the raw email subject + body.
                  Uses claude-haiku-4-5 with the rubric in a prompt-cached
                  system block so follow-up calls share the classifier's
                  instructions in cache.
  linear/         Minimal GraphQL client for Linear's issueCreate mutation.
                  Plain net/http + encoding/json — no generated SDK. Disabled
                  (ErrDisabled) when LINEAR_API_KEY is unset.
```

### Database Migrations

| Migration | Description |
|-----------|-------------|
| `001_initial.sql` | Users, cameras, sessions, API tokens, segments |
| `002_multi_user.sql` | Multi-user support |
| `003_audit_log.sql` | Audit log table (dropped in 011; see below) |
| `004_billing.sql` | Subscriptions table |
| `005_fk_cascade.sql` | Foreign key cascades |
| `006_ownership.sql` | Camera ownership |
| `007_hls_rewrite.sql` | HLS rewrite: provision tokens, commands queue, camera API keys, segment has_motion |
| `008_motion.sql` | Adds `has_motion` boolean column to `segments` table |
| `009_indexes.sql` | Adds `idx_segments_created_at` index for scalability |
| `010_cleanup.sql` | Drops dead tables/columns: sessions, owner, enrollment_tokens, cameras.cert_fingerprint |
| `011_drop_audit_log.sql` | Drops the `audit_log` table — nothing ever wrote to it; slog is the audit trail |
| `012_admins.sql` | Admin grants table |
| `013_user_soft_delete.sql` | Soft delete for users |
| `014_email_tokens.sql` | One-time email secrets (verify, reset, change-email, login OTP) |
| `015_support_tickets.sql` | Support email triage audit trail keyed on Svix message id |
| `016_camera_public_keys.sql` | `camera_public_keys` table (later inlined — see 017) |
| `017_inline_public_key.sql` | Inlines camera public key on `cameras` and drops stale API-key tables |
| `018_camera_fw_version.sql` | Adds `fw_version` text column to `cameras` for OTA firmware tracking |
| `019_recording_mode_never_default.sql` | Flips `cameras.recording_mode` default from `'constant'` to `'never'` (streaming-only) so new enrollments opt-in to recording |

## Viewer Structure

```
index.html             PWA head: favicon.svg, manifest.json, theme-color, apple-touch-icon
main.ts                Mounts App; registers /sw.js in production (skipped in dev for HMR)
public/
  favicon.svg          Ghost silhouette (transparent bg) used for browser tab icon
  icon.svg             App icon (rounded dark background + ghost)
  icon-maskable.svg    Maskable PWA icon (80% safe zone, full-bleed background)
  manifest.json        PWA manifest (name, start_url, standalone display, icons, theme color)
  sw.js                Service worker: network-first shell cache, skips /api, /hls, /events

signaling.ts           API calls: fetchCameras, fetchTelemetryRange, fetchCoverage
stores/
  transport.svelte.ts  SSE connection, auth state, camera polling
  cameras.svelte.ts    Camera registry, telemetry, online status
  scrubber.svelte.ts   Timeline mode (live/playback), playhead time, per-camera coverage data
                       Realtime coverage updates via SSE coverage event
  settings.svelte.ts   Theme, layout, mute state (localStorage)
  groups.svelte.ts     Group list + active group
  alerts.svelte.ts     Disconnect/reconnect notifications; handles motion and storage_capped alert types
  cameraConfig.svelte.ts  Display name overrides (localStorage)
  clip.svelte.ts          Clip mode state: enabled, startTime/endTime, phase, progress, seekRevision
                          5-min max, 10s min; toggle enters/exits clip mode
  billing.svelte.ts       Subscription, tiers, usage state + Stripe checkout/portal
                          Derived fields: storageUsedGB, storageLimitGB, storagePercent, isStorageCapped
components/
  HlsPlayer.svelte    hls.js wrapper for HLS playback via /hls/{deviceID}/live.m3u8 and vod.m3u8
                       Supports loop playback via loopStart/loopEnd props (rAF boundary clamping)
  TimelineScrubber.svelte  Timeline with union bar + selected camera overlay, per-camera coverage
                           Coverage bars merge regardless of hasMotion (motion is coloring only)
                           Map tracking on by default, re-engages on scrub/live
                           Clip mode: scissors button, yellow drag handles, auto-zoom on enter/exit
                           Zoom-on-hold-still: 1800ms delay, cancelled if mouse moves
  ClipDownloadBar.svelte   Download controls for clip mode: Video (MP4), CSV, JSON export buttons
                           Progress bar during download/processing, multi-camera support
  LiveView.svelte      Main view with empty-state onboarding watermark
  layout/StorageCapBanner.svelte  Persistent banner above the main view that surfaces tier storage state:
                                  amber warning at >=85%, red capped banner at 100%. Capped copy
                                  clarifies recording is paused but WebRTC live view still works.
                                  Upgrade button opens the settings dialog.
  camera/CameraCard.svelte  Camera card with HLS player, uses live.m3u8 / vod.m3u8
  camera/CameraList.svelte  Sidebar camera list with gear icon for settings dialog
  camera/CameraMarker.svelte  Dot always visible, info/pip panels float to top-right with overlap spreading
  camera/CameraSettingsDialog.svelte  Camera settings: Name, Resolution, Recording Mode, motion alerts toggle, delete camera
  camera/AddCameraDialog.svelte  Client-side QR SVG generation, shows provision token
```
