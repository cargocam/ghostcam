# Architecture Details

## Shared Types (`common/`)

```
types.go       PresignRequest, PresignResponse, PresignedUrl, UploadedSegment, ProvisionRequest/Response
               CameraCommand — server→camera commands delivered via telemetry poll response
               (set_resolution, set_recording_mode, reboot, unregister, network_config, remove_network)
telemetry.go   TelemetryDatagram — JSON payload with optional fields (CPU, temp, mem, GPS, wifi signal, uptime)
```

## Camera Structure

```
camera/            (package main — binary builds from this directory)
  main.go          Entrypoint: config, signal handling, goroutine orchestration (WaitGroup),
                   capture crash recovery with exponential backoff (1s→30s) and 5-minute stability threshold,
                   graceful shutdown (WaitGroup drain, 15s timeout). Pure orchestration — all logic in the
                   other camera/ files.
  config.go        CameraConfig + cameraConfigFile, layered TOML/env/CLI resolution
                   RecordingMode ("constant"/"motion") — runtime override via {dataDir}/recording_mode
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
                   Supports --provision-token CLI flag / GHOSTCAM_PROVISION_TOKEN env var for headless provisioning
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
                  cameraAuth (Bearer API key), adminAuth (viewerAuth + admin email check)
  ratelimit.go    Per-IP token bucket rate limiter with opportunistic eviction
                  (no dedicated cleanup goroutine)

  auth_handlers.go  Login (with DummyVerify timing equalization), Logout, Register (403),
                    ChangePassword. setAuthCookie uses Config.secureCookies().
  cameras.go        ListCameras / Enroll / GetCamera / UpdateCamera / DeleteCamera.
                    Enroll enforces tier camera limit (402 on exceed), UpdateCamera
                    enqueues commands for resolution/recording_mode changes.
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
  admin.go          ReloadConfig, FirmwareLatest, FirmwareUpload, GithubWebhook, QueryAudit
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
```

### Database Migrations

| Migration | Description |
|-----------|-------------|
| `001_initial.sql` | Users, cameras, sessions, API tokens, segments |
| `002_multi_user.sql` | Multi-user support |
| `003_audit_log.sql` | Audit log table |
| `004_billing.sql` | Subscriptions table |
| `005_fk_cascade.sql` | Foreign key cascades |
| `006_ownership.sql` | Camera ownership |
| `007_hls_rewrite.sql` | HLS rewrite: provision tokens, commands queue, camera API keys, segment has_motion |
| `008_motion.sql` | Adds `has_motion` boolean column to `segments` table |
| `009_indexes.sql` | Adds `idx_segments_created_at` index for scalability |
| `010_cleanup.sql` | Drops dead tables/columns: sessions, owner, enrollment_tokens, cameras.cert_fingerprint |

## Viewer Structure

```
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
  camera/CameraCard.svelte  Camera card with HLS player, uses live.m3u8 / vod.m3u8
  camera/CameraList.svelte  Sidebar camera list with gear icon for settings dialog
  camera/CameraMarker.svelte  Dot always visible, info/pip panels float to top-right with overlap spreading
  camera/CameraSettingsDialog.svelte  Camera settings: Name, Resolution, Recording Mode, motion alerts toggle, delete camera
  camera/AddCameraDialog.svelte  Client-side QR SVG generation, shows provision token
```
