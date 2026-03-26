# server

Ghostcam server binary. Accepts camera QUIC connections, stores telemetry in Redis, relays live feeds via WebRTC, serves recorded HLS segments, and exposes a REST + SSE HTTP API consumed by the browser viewer.

Supports TOML config files and environment variables with layered resolution: defaults -> config file -> env vars. Config files are optional — env-var-only deployments (Docker) still work.

## Configuration

### Config File Search Order

1. `$GHOSTCAM_CONFIG_FILE` (env var)
2. `$GHOSTCAM_DATA_DIR/server.toml`
3. `/etc/ghostcam/server.toml`

See `server.example.toml` in the repo root for all available settings with comments.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GHOSTCAM_CONFIG_FILE` | _(none)_ | Explicit path to TOML config file |
| `GHOSTCAM_DATA_DIR` | `/var/ghostcam` | Data directory: CA keys, server cert |
| `GHOSTCAM_DATABASE_URL` | _(required)_ | PostgreSQL connection URL (env-only, cannot be in config file) |
| `GHOSTCAM_PUBLIC_IP` | _(none)_ | Public IP for WebRTC ICE candidates. **Must be a reachable LAN IP** — `127.0.0.1` breaks Firefox. If unset, derived from HTTP Host header. |
| `GHOSTCAM_HTTP_PORT` | `3000` | HTTP API + static assets |
| `GHOSTCAM_QUIC_PORT` | `4433` | QUIC ingest for cameras |
| `GHOSTCAM_WEBRTC_PORT` | `3478` | Shared WebRTC UDP port |
| `GHOSTCAM_REDIS_URL` | _(none)_ | Redis connection URL. If unset or empty, telemetry storage and the telemetry API are disabled. |
| `GHOSTCAM_ENROLLMENT_ADDR` | `<public_ip>:<quic_port>` | Address in enrollment JWTs |
| `GHOSTCAM_ADMIN_EMAIL` | `admin@localhost` | Admin account email |
| `GHOSTCAM_ADMIN_PASSWORD` | _(auto-generated)_ | Preset admin password (env-only) |
| `GHOSTCAM_HMAC_KEY` | `dev-hmac-key` | Secret key for HMAC-SHA256 audit log signing. Set to a strong random value in production. |

### Runtime Reload

Configuration can be reloaded without restart:
- **SIGHUP signal**: `kill -HUP <pid>`
- **API endpoint**: `POST /api/v1/admin/reload` (requires auth)

Non-reloadable settings (ports, database_url, data_dir) log warnings but take effect only after restart.

On first start the server creates `GHOSTCAM_DATA_DIR`, runs PostgreSQL migrations, generates a CA keypair, self-signs a server TLS certificate, and prints an initial password for the `admin` account.

## Module Map

| Module | Purpose |
|--------|---------|
| `main` | Config load, bootstrap, task spawning, SIGHUP handler, Axum server |
| `config` | `ServerConfig` + `ServerConfigFile`, layered TOML/env resolution |
| `db_trait` | `Database` trait + record types (`CameraRecord`, `SessionRecord`, `ApiTokenRecord`, etc.) |
| `db` | `PostgresDatabase` — PostgreSQL implementation of `Database` via `sqlx` |
| `auth` | Token hashing, HMAC verification, session validation helpers |
| `audit` | `AuditLogger` — HMAC-SHA256 signed append-only audit log |
| `frames` | Re-exports `ghostcam::wire::frames::*` (local shim) |
| `sse` | `SseEventBus` — per-user broadcast channels for Server-Sent Events |
| `api` | Axum HTTP routes. See [`api/README.md`](src/api/README.md) |
| `ingest` | QUIC camera ingest pipeline. See [`ingest/README.md`](src/ingest/README.md) |
| `egress` | WebRTC egress (str0m). See [`egress/README.md`](src/egress/README.md) |
| `pki` | CA, enrollment, revocation. See [`pki/README.md`](src/pki/README.md) |
| `redis` | Telemetry storage and query. See [`redis/README.md`](src/redis/README.md) |
