# Configuration

Both server and camera support TOML config files with layered resolution. Environment variables and CLI flags always take precedence. Config files are **optional** -- the env-var-only workflow still works (Docker uses this).

## Layering Order

**Server**: defaults -> config file -> env vars
**Camera**: defaults -> config file -> env vars -> CLI flags

## Config File Search Paths

**Server** (first found wins):
1. `$GHOSTCAM_CONFIG_FILE`
2. `$GHOSTCAM_DATA_DIR/server.toml`
3. `/etc/ghostcam/server.toml`

**Camera** (first found wins):
1. `--config <path>` CLI flag
2. `$GHOSTCAM_CONFIG_FILE`
3. `$GHOSTCAM_DATA_DIR/camera.toml`
4. `/boot/ghostcam.conf` (backward compatible -- valid TOML key=value format)

## Sensitive Fields

`database_url` and `admin_password` are **env-var only**. They cannot be set in the TOML config file.

Config is loaded once at startup — there is no runtime reload endpoint. To apply a config change, restart the server.

## Environment Variables

### Server

| Variable | Default | Description |
|----------|---------|-------------|
| `GHOSTCAM_CONFIG_FILE` | _(none)_ | Explicit config file path |
| `GHOSTCAM_DATA_DIR` | `/var/ghostcam` | Data directory |
| `GHOSTCAM_DATABASE_URL` | _(required)_ | PostgreSQL URL |
| `GHOSTCAM_REDIS_URL` | _(none)_ | Redis URL (telemetry streams, SSE pub/sub) |
| `GHOSTCAM_HTTP_PORT` | `3000` | HTTP port |
| `GHOSTCAM_STATIC_DIR` | _(none)_ | Directory for serving static UI files |
| `GHOSTCAM_ADMIN_EMAIL` | `admin@localhost` | Bootstrap admin email. Used on first run to seed a normal user and grant them a row in the `admins` table. On subsequent runs also used to backfill the admins table if a pre-existing user with this email isn't an admin yet. |
| `GHOSTCAM_ADMIN_PASSWORD` | _(auto-generated)_ | Bootstrap admin password (first-run only) |
| `GHOSTCAM_PUBLIC_URL` | _(none)_ | Public URL for QR codes and CORS origin |
| `GHOSTCAM_S3_BUCKET` | `ghostcam-segments` | S3/Tigris bucket name |
| `GHOSTCAM_S3_REGION` | `auto` | S3 region |
| `GHOSTCAM_S3_ENDPOINT` | _(none)_ | S3 endpoint URL (Tigris, MinIO, etc.) |
| `GHOSTCAM_S3_PRESIGN_TTL_SECS` | `3600` | Presigned URL TTL in seconds |
| `GHOSTCAM_SEGMENT_RETENTION_DAYS` | `30` | Segment retention in days. Used as the cutoff for opportunistic prune in the presign handler and as the read cutoff for manifest / coverage queries. |
| `STRIPE_SECRET_KEY` | **required** | Stripe API key — server won't start without it |
| `STRIPE_WEBHOOK_SECRET` | _(none)_ | Stripe webhook signing secret (signature verification skipped when unset — safe for dev) |
| `STRIPE_PORTAL_CONFIG_ID` | _(none)_ | Portal config with plan switching |
| `GITHUB_WEBHOOK_SECRET` | _(none)_ | HMAC-SHA256 secret for `POST /api/v1/webhooks/github`. Required in production (any server with `GHOSTCAM_PUBLIC_URL` set) — the webhook 403s when unset. See "GitHub release webhook" below. |
| `RESEND_API_KEY` | _(none)_ | Resend API key for transactional email |
| `RESEND_FROM_EMAIL` | _(none)_ | Sender address, e.g. `Ghostcam <noreply@ghostcam.app>` |
| `RESEND_REPLY_TO` | _(none)_ | Optional reply-to address |
| `RESEND_WEBHOOK_SECRET` | _(none)_ | Svix webhook secret (`whsec_...`) for `POST /api/v1/webhooks/resend`. Required in production — the webhook 403s when unset. See "Support email triage" below. |
| `ANTHROPIC_API_KEY` | _(none)_ | Claude API key for support-email triage classification. When empty, the raw email subject + body is posted to Linear as-is. |
| `LINEAR_API_KEY` | _(none)_ | Linear personal API key for creating support tickets. When empty, the Linear step is a logging no-op. |
| `LINEAR_TEAM_ID` | _(none)_ | Linear team UUID that support tickets are created in. Required when `LINEAR_API_KEY` is set. |

### Camera

| Variable | Default | Description |
|----------|---------|-------------|
| `GHOSTCAM_CONFIG_FILE` | _(none)_ | Explicit config file path |
| `GHOSTCAM_DATA_DIR` | `/var/ghostcam` | Data directory |
| `GHOSTCAM_SERVER_URL` | _(from provisioning)_ | Server HTTPS URL |
| `GHOSTCAM_AUDIO_DEVICE` | `default` | ALSA audio input device name |
| `GHOSTCAM_LOCAL_STORAGE_CAP_MB` | `4096` | Local segment storage cap in MB |
| `GHOSTCAM_SEGMENT_DIR` | _(none)_ | Directory for segment ring buffer |
| `GHOSTCAM_VIDEO_PROFILE` | _(none)_ | Video preset: `zero2w`/`480p`, `pi4`/`720p`, `pi5`/`1080p` |
| `GHOSTCAM_VIDEO_WIDTH` | _(from profile)_ | Custom video width |
| `GHOSTCAM_VIDEO_HEIGHT` | _(from profile)_ | Custom video height |
| `GHOSTCAM_VIDEO_FPS` | _(from profile)_ | Frames per second |
| `GHOSTCAM_VIDEO_BITRATE` | _(from profile)_ | Bitrate in bps |
| `GHOSTCAM_VIDEO_KEYFRAME_INTERVAL` | _(from profile)_ | Keyframe interval in frames |
| `GHOSTCAM_PROVISION_TOKEN` | _(none)_ | Provision token for headless provisioning |

## Billing Tiers

Billing is always enabled and Stripe is required. Every user defaults to **free**.
`effectiveTier()` derives the tier from Stripe subscription state; users
without an active subscription get the free tier.

Paid tiers are **not hardcoded**. On startup — and on every relevant Stripe
webhook — the server calls `prices.list(active=true, expand=data.product)`
and registers every Stripe product that carries either of these
product-level metadata keys as a tier:

| Metadata key | Value |
|--------------|-------|
| `ghostcam_camera_limit` | integer (e.g. `4`) or `unlimited` |
| `ghostcam_storage_gb`   | integer (e.g. `50`) or `unlimited` |

Products missing both keys are skipped with a `billing: skipping stripe
product without ghostcam metadata` warning — fail-closed so a new dashboard
product can't accidentally grant paid limits. The tier's display name is
pulled from the Stripe product `name`, and the price / currency / interval
come from the Stripe price. The free tier is the one exception — it's a
compile-time constant (`billing.FreeTier`) with 1 camera and 5 GB.

### Required Stripe webhook events

Configure your Stripe webhook endpoint (`/api/v1/webhooks/stripe`) to
forward at minimum:

| Event | Purpose |
|-------|---------|
| `checkout.session.completed`      | Record a new paid subscription on the user. |
| `customer.subscription.updated`   | Handle plan switches, renewals, and status flips. |
| `customer.subscription.deleted`   | Downgrade the user back to free on cancellation. |
| `product.created`/`updated`/`deleted` | Refresh the server's tier cache so dashboard edits are visible in real time rather than waiting for the hourly background refresh. |
| `price.created`/`updated`/`deleted`   | Same — a new price on an existing product should become a tier option immediately. |

The server runs the tier refresh asynchronously in a 15-second context so
a slow Stripe API call never blocks webhook delivery (Stripe would retry
on timeout and risk reordering events). If the refresh fails, the existing
cache contents are preserved and the failure is logged; the next webhook
delivery will try again.

### Manual tier refresh

Server startup is the only unconditional refresh — there is no hourly
background tick. The cache is kept up to date by three mechanisms, all
reactive:

1. **Server startup** (one-shot synchronous refresh via `main.run`).
2. **Stripe webhooks** for product/price lifecycle events.
3. **`POST /api/v1/billing/tiers/refresh`** — an authenticated,
   rate-limited endpoint the UI's settings-dialog Retry button calls
   when the tier list is empty. Lets a user who just tagged product
   metadata in the Stripe dashboard see it immediately without
   waiting for a webhook to land.

This is deliberate: the server has no long-running goroutines (see the
retention/cleanup table below for the same pattern), and billing is not
load-bearing enough for one.

## GitHub Release Webhook (Pi Image Publishing)

Pi device images (`ghostcam-{zero2w,pi4,pi5}-{tag}.img.xz`) are published
to S3 automatically when a GitHub release happens — the release workflow
already uploads them to the GitHub Release, and the server listens on
`POST /api/v1/webhooks/github` for the `release.published` event, pulls
the matching assets, uploads each to `firmware/{version}/ghostcam-{device}.img.xz`,
and writes per-device metadata to Redis (`firmware:images:{device}`).
The "Get Started" onboarding card calls `GET /api/v1/firmware/images` to
offer viewers a direct download.

**One-time repo setup** (required on each server where you want
automatic ingestion):

1. Choose a strong random value for `GITHUB_WEBHOOK_SECRET` and set it
   in the server environment.
2. In the `cargocam/ghostcam` GitHub repo → **Settings → Webhooks → Add
   webhook**.
3. Payload URL: `https://<your server>/api/v1/webhooks/github`.
4. Content type: `application/json`.
5. Secret: the value from step 1.
6. Events: "Let me select individual events" → tick **Releases** only.
7. Click "Send ping" and confirm a 200 response; the server logs
   `github webhook: pi image ingestion complete` on the first real
   release.

With `GITHUB_WEBHOOK_SECRET` unset, the webhook returns `403` on any
deployed server (`GHOSTCAM_PUBLIC_URL` set) — fail-closed. On a local
dev server without `GHOSTCAM_PUBLIC_URL`, deliveries are accepted
without signature validation so a developer can replay payloads against
a laptop server.

## Transactional Email (Resend)

The server sends transactional emails via [Resend](https://resend.com) for
auth flows: email verification, password reset, email change confirmation,
password-changed notifications, and login OTP codes.

Email is **optional**. When `RESEND_API_KEY` is empty, the mailer logs what
it would have sent (including full links and OTP codes) to stdout so
development and self-hosted deployments work without any Resend
configuration. Set the three env vars below to enable real sends:

| Variable | Required | Description |
|----------|----------|-------------|
| `RESEND_API_KEY` | yes | Resend API key |
| `RESEND_FROM_EMAIL` | yes | Sender address, e.g. `Ghostcam <noreply@ghostcam.app>`. The domain must be verified in Resend. |
| `RESEND_REPLY_TO` | no | Optional reply-to address |

Templates live in `server/mailer/templates/` as paired `.html` / `.txt`
files embedded via `//go:embed`. Both HTML and plain-text parts are sent
with every email for maximum deliverability.

### Email-backed auth flows

| Flow | Endpoint | Token TTL | Notes |
|------|----------|-----------|-------|
| Email verification | `POST /api/v1/auth/verify-email` | 24 hours | Link sent on admin-created user signup |
| Password reset | `POST /api/v1/auth/forgot-password` → link → `POST /api/v1/auth/reset-password` | 1 hour | Always returns 200 (prevents enumeration) |
| Email change | `PATCH /api/v1/auth/email` → confirmation link → `POST /api/v1/auth/email/confirm` | 24 hours | Requires current password; confirmation sent to new address |
| Login OTP | `POST /api/v1/auth/otp/request` → code → `POST /api/v1/auth/otp/verify` | 10 minutes | 6-digit code, max 5 attempts before invalidation |

All tokens and OTP codes are stored as HMAC-SHA256 hashes in the
`email_tokens` table (same pattern as `api_tokens`). Raw values only exist
in the email body.

## Support Email Triage (Resend Inbound → Claude → Linear)

Inbound customer support email (e.g. `support@ghostcam.app`) is pushed
by Resend to `POST /api/v1/webhooks/resend`, classified by Claude, and
filed as a Linear issue. Every stage is independently gated on its own
API key — absent keys degrade to a logging no-op so local dev needs
nothing extra.

| Env var | Purpose |
|---------|---------|
| `RESEND_WEBHOOK_SECRET` | Svix webhook secret (`whsec_...`). Required in production; the webhook 403s without it. |
| `ANTHROPIC_API_KEY` | Claude API key for classification. Without it the raw subject + body are posted to Linear unchanged. |
| `LINEAR_API_KEY` | Linear personal API key for `issueCreate`. Without it the step is a no-op (row is marked `failed`). |
| `LINEAR_TEAM_ID` | Linear team UUID the ticket is created in. Required when `LINEAR_API_KEY` is set. |

Every inbound delivery is persisted to `support_tickets` (migration
`015`) keyed on `svix-id`, so a Resend redelivery is idempotent and
operators have an audit trail with the raw email text, classification,
and Linear URL.

Setup steps (production):

1. In the Resend dashboard, verify an inbound domain and add an
   inbound route (e.g. `support@<your-domain>`) whose webhook
   destination is `https://<your-public-host>/api/v1/webhooks/resend`.
2. Copy the route's `whsec_` secret into `RESEND_WEBHOOK_SECRET`.
3. Create an Anthropic API key and set `ANTHROPIC_API_KEY`.
4. Create a Linear personal API key, find your team's UUID (Settings
   → API → "My account" drop-down), set `LINEAR_API_KEY` and
   `LINEAR_TEAM_ID`.
5. Restart the server (config is loaded once at startup; see
   "Sensitive Fields" above).

Send a test email to the inbound address and confirm:
- a row appears in `support_tickets` with `status='routed'`;
- a Linear issue is created with `category`, `priority`, and the
  verbatim email body in the description;
- a second (Resend redelivery-style) POST with the same `svix-id`
  returns `{status: "duplicate"}` and does **not** create a second
  Linear issue.

`support_tickets.status` has four values so operators can tell the
stages apart when querying the audit trail:

| `status` | Meaning |
|----------|---------|
| `received` | Row was inserted but async triage hasn't run yet (or was deferred because the in-flight cap was hit). |
| `classified` | Triage succeeded but Linear was intentionally unconfigured (`LINEAR_API_KEY` unset). Populated category/priority/title are preserved so a later backfill can reroute. |
| `routed` | Linear issue created; `linear_issue_url` is set. |
| `failed` | Linear call errored (network/auth/graphql). `error` column carries the message. |

## Retention & Cleanup

The server has **no background cleanup goroutines**. All cleanup is driven by
normal request activity or by the storage layer itself:

| Concern | Mechanism |
|---------|-----------|
| Sessions | Removed — auth is stateless (JWT cookies + API tokens). |
| S3 segment objects + DB rows (retention) | Pruned together, opportunistically, inside the presign handler whenever a camera confirms uploads. `PruneSegments` deletes DB rows `WHERE device_id = $1 AND created_at < cutoff LIMIT 100` and returns the deleted rows so the handler can issue matching S3 deletes. We deliberately do **not** use an S3 bucket lifecycle rule because firmware binaries live in the same bucket under `firmware/` and must not be auto-expired — a camera that stays offline beyond the retention window would otherwise lose its OTA update path. |
| User-initiated deletion | `DELETE /api/v1/cameras/:id/footage` (optionally scoped by `from_ms`/`to_ms`) runs the same `DeleteSegmentsRange` + S3 reap loop in bounded batches; the UI drives it to completion via the `has_more` flag. `DELETE /api/v1/cameras/:id` now runs this purge synchronously before removing the cameras row so camera deletion no longer orphans S3 objects. |
| HLS / coverage reads | `ListSegments` / `ListSegmentCoverage` clamp their `from` parameter to `now - retentionMs`, so rows that haven't been pruned yet never surface through the API. |
| Expired provision tokens | Deleted in the same transaction that creates a new token for the same user. |
| Camera commands | `ClaimCommands` uses `DELETE ... RETURNING`, so the queue can't grow past what's claimed on the next telemetry poll. |
| Expired API tokens | Dropped on the next verify attempt by `VerifyAPIToken`. |
| Rate-limit entries | Evicted opportunistically when the in-memory map grows past a threshold. |
| Stale unclaimed cameras | Not possible — `CreateProvisionedCamera` always requires a `user_id`, so an unclaimed camera row can never be created. |
