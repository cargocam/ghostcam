# API Reference

Auth: `Authorization: Bearer <token>` or `ghostcam-token=<jwt>` cookie. Cookies use `Secure` flag when `GHOSTCAM_PUBLIC_URL` starts with `https://`.

## Auth

```
POST   /api/v1/auth/register               DISABLED (returns 403 registration_disabled)
POST   /api/v1/auth/login                  { email, password } → JWT cookie (rate limited: 10/min per IP)
POST   /api/v1/auth/logout                 Clears JWT cookie
PATCH  /api/v1/auth/password               { current_password, new_password }
POST   /api/v1/auth/forgot-password        { email } → always 200 (rate limited: 5/min per IP)
POST   /api/v1/auth/reset-password         { token, new_password }
POST   /api/v1/auth/verify-email           { token }
POST   /api/v1/auth/verify-email/resend    (authenticated, unverified users only)
PATCH  /api/v1/auth/email                  { new_email, current_password } (authenticated)
POST   /api/v1/auth/email/confirm          { token }
POST   /api/v1/auth/otp/request            { email } → always 200 (rate limited: 5/min per IP)
POST   /api/v1/auth/otp/verify             { email, code } → JWT cookie (rate limited: 10/min per IP)
```

## Cameras

```
GET    /api/v1/cameras                     List cameras (includes provisioned bool, telemetry if available)
POST   /api/v1/cameras                     Generate provision token (returns 402 when tier camera limit reached)
POST   /api/v1/cameras/enroll/qr           Returns JSON {payload, token, expires_at} + optional WiFi
GET    /api/v1/cameras/enroll/qr           Same as POST with defaults (24h TTL, no WiFi)
GET    /api/v1/cameras/:id                 Camera details
PATCH  /api/v1/cameras/:id                 Update name/notes/resolution/recording_mode
DELETE /api/v1/cameras/:id                 Delete camera (synchronously purges S3 segments first,
                                           then removes the cameras row — the DB cascade takes care
                                           of segments, api keys, enrollment tokens).
DELETE /api/v1/cameras/:id/footage         Delete footage. Query params (optional):
                                           ?from_ms=&to_ms= ; omitting both deletes every segment
                                           for the camera. Returns { deleted_count, bytes_freed,
                                           has_more }. Bounded to ~2000 segments per call so the UI
                                           must re-invoke while has_more is true.

POST   /api/v1/cameras/:id/telemetry       Camera telemetry POST (camera auth) → returns pending commands
POST   /api/v1/cameras/:id/presign         Request presigned S3 URLs + confirm uploads (camera auth)
GET    /api/v1/cameras/:id/live            WebSocket upgrade for live H.264 relay (camera auth)
POST   /api/v1/cameras/provision            Camera provisioning with one-time token (rate limited: 10/min per IP)
```

## WebRTC (WHEP)

Low-latency live viewing via WebRTC. The server acts as an ICE-lite SFU
relaying H.264 from the camera's WebSocket to viewers.

```
POST   /api/v1/whep/:deviceID              SDP offer (application/sdp) → SDP answer + Location header
DELETE /api/v1/whep/:deviceID/:sessionID    Tear down viewer session
```

### Camera Live WebSocket Protocol

Camera connects to `GET /api/v1/cameras/:id/live` (WebSocket upgrade) with bearer auth.

**Text frames** (JSON control messages):
- Camera → Server: `{"type": "ready"}` (sent after connect)
- Server → Camera: `{"type": "start_stream"}` (first viewer connected)
- Server → Camera: `{"type": "stop_stream"}` (last viewer disconnected)

**Binary frames** (media, camera → server only):
- Bytes 0-3: timestamp (uint32 big-endian, ms since arbitrary epoch)
- Byte 4: flags (bit 0 = is_keyframe/video, bit 1 = is_audio)
- Bytes 5+: payload (H.264 NAL unit when !is_audio, Opus packet when is_audio)

Audio: ffmpeg encodes ALSA input to Opus (32kbps, low-delay, 20ms frames)
alongside the AAC used for HLS segments. Opus adds ~4 KB/s to the WebSocket.

## Telemetry

```
GET    /api/v1/telemetry/:id/latest        Most recent telemetry from Redis
GET    /api/v1/telemetry/:id               ?from=<ms>&to=<ms>&limit=<n> — historical telemetry range
GET    /api/v1/telemetry/:id/export        ?from=&to=&format=csv|json → telemetry export (Content-Disposition attachment)
```

## HLS

```
GET    /hls/:id/live.m3u8                  Live HLS manifest (90s sliding window, no ENDLIST, hls.js polls)
GET    /hls/:id/vod.m3u8                   VOD HLS manifest (?from=&to=, max 24h range, LIMIT 2000, with ENDLIST)
GET    /hls/:id/init.mp4                   Init segment → 307 redirect to S3
GET    /hls/:id/:segmentID.ts              Segment → 302 redirect to S3 (presigned on the fly)
GET    /hls/:id/coverage                   Segment coverage with motion flags (has_motion always present)
```

## Clips

```
POST   /api/v1/clips/prepare              { device_id, from_ms, to_ms } → presigned segment URLs for clip download
```

## Events

```
GET    /events                             SSE stream (telemetry, motion, storage_capped, coverage, camera_limit_exceeded)

GET    /api/v1/events                      List events with pagination
GET    /api/v1/events/unread               Get unread event count
PATCH  /api/v1/events/:id/read             Mark single event as read
POST   /api/v1/events/read-all             Mark all events as read
DELETE /api/v1/events/:id                  Dismiss/delete an event
```

## Tokens

```
GET    /api/v1/tokens                      List API tokens
POST   /api/v1/tokens                      Create token
DELETE /api/v1/tokens/:id                  Revoke token
```

## Billing

```
GET    /api/v1/billing/subscription         Returns { tier }
GET    /api/v1/billing/tiers               Available tiers with limits (public)
POST   /api/v1/billing/checkout            { tier, success_url, cancel_url } → { url } (Stripe Checkout Session)
POST   /api/v1/billing/portal              { return_url } → { url } (Stripe Billing Portal)
GET    /api/v1/billing/usage               Storage + camera usage for current user
POST   /api/v1/webhooks/stripe             Stripe webhook: checkout.session.completed, subscription.updated, subscription.deleted
POST   /api/v1/webhooks/github              GitHub release webhook: ingests ghostcam-{device}-{version}.img.xz assets into S3 (public, HMAC-validated)
```

## Firmware & Pi images

```
GET    /api/v1/firmware/latest             Latest camera firmware binary + presigned download URL (public, no auth)
GET    /api/v1/firmware/images             Available Pi device images (zero2w / pi4 / pi5) with presigned download URLs
POST   /api/v1/admin/firmware              Upload firmware binary to Tigris — admin only
```

### GitHub release webhook

The server's `POST /api/v1/webhooks/github` endpoint is the ingestion path
for Pi device images. It is intended to be called by a GitHub repository
webhook configured on the `Releases` event, not by humans.

- **Authentication:** `X-Hub-Signature-256: sha256=<hex>` is required and
  validated as HMAC-SHA256 over the raw request body using
  `GITHUB_WEBHOOK_SECRET`. Mismatch → `401`. Missing secret on a deployed
  server (`GHOSTCAM_PUBLIC_URL` set) → `403`.
- **Events:**
  - `release.published` — server iterates `release.assets` and ingests every
    asset matching `ghostcam-{zero2w|pi4|pi5}-{tag_name}.img.xz`: downloads
    from `browser_download_url`, uploads to S3 at
    `firmware/{version}/ghostcam-{device}.img.xz`, and writes JSON
    `{version, size_bytes, sha256}` to Redis key `firmware:images:{device}`.
    Response body: `{ingested: [{device, version, size_bytes, sha256}, ...]}`.
  - `ping` — replies with `{pong: "ghostcam"}`.
  - Any other event type is accepted and ignored with `200`.

### `GET /api/v1/firmware/images`

Returns the current `PiImagesResponse` — one `PiImage` per device that has
a published image. Fields: `device`, `version`, `download_url` (presigned
S3 GET, TTL per `GHOSTCAM_S3_PRESIGN_TTL_SECS`), `size_bytes`, `sha256`.

## Health

```
GET    /healthz                            Always 200 (no auth)
GET    /readyz                             200 when ready (no auth)
```

## SSE Event Types

The `/events` endpoint delivers events via per-user Redis pub/sub channels:

| Event | Payload | Description |
|-------|---------|-------------|
| `telemetry` | `{ device_id, telemetry }` | Realtime telemetry from Redis Streams (XREAD) |
| `motion_detected` | `{ device_id, segment_id, start_ts, end_ts }` | Motion detected in a recording segment |
| `storage_capped` | `{ user_id, device_id, storage_bytes, limit_gb }` | User's storage exceeds tier limit; uploads paused |
| `coverage` | `{ device_id, segment }` | New segment coverage available (realtime timeline updates) |

SSE connections use `http.NewResponseController` to disable the write deadline for long-lived connections.

## Camera-Server Protocol

All communication is plain HTTPS. Cameras authenticate with a Bearer API key obtained during provisioning.

### Telemetry Poll (camera → server, every 10s)

`POST /api/v1/cameras/{deviceID}/telemetry` with JSON `TelemetryDatagram` body. Response contains an array of `CameraCommand` objects (piggy-backed commands).

### CameraCommand (server → camera, via telemetry response)

JSON objects with a `"type"` field: `set_resolution { resolution }`, `set_recording_mode { mode }`, `reboot`, `unregister`, `network_config { ssid, psk }`, `remove_network { ssid }`

`set_resolution` and `set_recording_mode` are persisted to disk by the camera (`{dataDir}/resolution`, `{dataDir}/recording_mode`) and trigger a process exit (systemd restarts with new config). `unregister` clears credentials and exits (systemd restarts, camera re-provisions).

### Segment Upload (camera → S3 → server confirmation)

1. Camera requests presigned PUT URLs: `POST /api/v1/cameras/{deviceID}/presign` with `{ count, uploaded[] }`
2. Camera uploads MPEG-TS segments directly to S3 via presigned PUT URL
3. Uploaded segment metadata is confirmed in the next presign request's `uploaded[]` array
4. Server records segments in Postgres and publishes motion events via Redis

### Telemetry Datagram

JSON-encoded with optional fields. Sent every 10s. Fields: `ts` (unix ms), `cpu` (%), `mem` (MB), `temp` (C), `uptime` (s), `lat`, `lon`, `alt`, `gps_fix`, `sig` (WiFi dBm).
