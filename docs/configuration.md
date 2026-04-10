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
| `GHOSTCAM_ADMIN_EMAIL` | `admin@localhost` | Admin email |
| `GHOSTCAM_ADMIN_PASSWORD` | _(auto-generated)_ | Preset admin password |
| `GHOSTCAM_PUBLIC_URL` | _(none)_ | Public URL for QR codes and CORS origin |
| `GHOSTCAM_S3_BUCKET` | `ghostcam-segments` | S3/Tigris bucket name |
| `GHOSTCAM_S3_REGION` | `auto` | S3 region |
| `GHOSTCAM_S3_ENDPOINT` | _(none)_ | S3 endpoint URL (Tigris, MinIO, etc.) |
| `GHOSTCAM_S3_PRESIGN_TTL_SECS` | `3600` | Presigned URL TTL in seconds |
| `GHOSTCAM_SEGMENT_RETENTION_DAYS` | `30` | Segment retention in days. Used as the cutoff for opportunistic prune in the presign handler and as the read cutoff for manifest / coverage queries. |
| `STRIPE_SECRET_KEY` | _(none)_ | Stripe API key |
| `STRIPE_WEBHOOK_SECRET` | _(none)_ | Stripe webhook signing secret |
| `STRIPE_PRICE_ID_STARTER` | _(none)_ | Stripe Price ID for starter tier |
| `STRIPE_PRICE_ID_PRO` | _(none)_ | Stripe Price ID for pro tier |
| `STRIPE_PRICE_ID_ENTERPRISE` | _(none)_ | Stripe Price ID for enterprise tier |
| `STRIPE_PORTAL_CONFIG_ID` | _(none)_ | Portal config with plan switching |

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

Billing is always enabled. Every user defaults to **free**. `effectiveTier()` derives tier from Stripe subscription state; when Stripe not configured (dev), returns "enterprise" (unlimited).

| Tier | Storage | Cameras |
|------|---------|---------|
| Free | 5 GB | 1 |
| Starter | 50 GB | 4 |
| Pro | 500 GB | 16 |
| Enterprise | unlimited | unlimited |

## Retention & Cleanup

The server has **no background cleanup goroutines**. All cleanup is driven by
normal request activity or by the storage layer itself:

| Concern | Mechanism |
|---------|-----------|
| Sessions | Removed — auth is stateless (JWT cookies + API tokens). |
| S3 segment objects + DB rows | Pruned together, opportunistically, inside the presign handler whenever a camera confirms uploads. `PruneSegments` deletes DB rows `WHERE device_id = $1 AND created_at < cutoff LIMIT 100` and returns the deleted rows so the handler can issue matching S3 deletes. We deliberately do **not** use an S3 bucket lifecycle rule because firmware binaries live in the same bucket under `firmware/` and must not be auto-expired — a camera that stays offline beyond the retention window would otherwise lose its OTA update path. |
| HLS / coverage reads | `ListSegments` / `ListSegmentCoverage` clamp their `from` parameter to `now - retentionMs`, so rows that haven't been pruned yet never surface through the API. |
| Expired provision tokens | Deleted in the same transaction that creates a new token for the same user. |
| Camera commands | `ClaimCommands` uses `DELETE ... RETURNING`, so the queue can't grow past what's claimed on the next telemetry poll. |
| Expired API tokens | Dropped on the next verify attempt by `VerifyAPIToken`. |
| Rate-limit entries | Evicted opportunistically when the in-memory map grows past a threshold. |
| Stale unclaimed cameras | Not possible — `CreateProvisionedCamera` always requires a `user_id`, so an unclaimed camera row can never be created. |
