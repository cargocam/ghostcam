# server

Ghostcam server binary. Accepts camera QUIC connections, stores telemetry in Redis, relays live feeds via WebRTC, serves recorded HLS segments, and exposes a REST + SSE HTTP API consumed by the browser viewer.

Configured entirely via environment variables — no config file.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GHOSTCAM_DATA_DIR` | `/var/ghostcam` | Data directory: CA keys, server cert |
| `GHOSTCAM_DATABASE_URL` | _(required)_ | PostgreSQL connection URL, e.g. `postgres://ghostcam:pw@localhost:5432/ghostcam` |
| `GHOSTCAM_PUBLIC_IP` | `127.0.0.1` | Public IP advertised in WebRTC ICE candidates. **Must be a reachable LAN IP** — `127.0.0.1` breaks Firefox because Firefox binds UDP on the LAN interface and cannot route to loopback from there. |
| `GHOSTCAM_HTTP_PORT` | `3000` | HTTP API + static assets |
| `GHOSTCAM_QUIC_PORT` | `4433` | QUIC ingest for cameras |
| `GHOSTCAM_REDIS_URL` | _(none)_ | Redis connection URL. If unset or empty, telemetry storage and the telemetry API are disabled. |

On first start the server creates `GHOSTCAM_DATA_DIR`, runs PostgreSQL migrations, generates a CA keypair, self-signs a server TLS certificate, and prints an initial password for the `admin` account.

## Module Map

| Module | Purpose |
|--------|---------|
| `main` | Env-var config, bootstrap, task spawning, Axum server |
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
