# server-solo
Single-operator deployment binary that wires `server-core` to a local SQLite database.

## Purpose
`server-solo` is the currently complete runtime deployment target:
- boots DB + PKI,
- listens for camera QUIC ingest,
- serves HTTP APIs for UI and automation clients,
- optionally enables Redis-backed telemetry history.

## Startup sequence (`src/main.rs`)
1. Read environment configuration.
2. Open SQLite DB (`<data_dir>/ghostcam.db`) and run migrations.
3. Initialize first-run secrets:
   - owner password (printed once),
   - HMAC secret.
4. Bootstrap PKI (`ca.crt`, `ca.key`, `server.crt`, `server.key`).
5. Initialize optional Redis manager + reconnect loop.
6. Build shared `AppState`.
7. Start QUIC accept loop for camera ingest.
8. Start Axum HTTP server with CORS + tracing layers.
9. Handle Ctrl-C graceful shutdown.

## Environment variables
- `GHOSTCAM_DATA_DIR` (default `/var/ghostcam`)
- `GHOSTCAM_PUBLIC_IP` (default `127.0.0.1`)
- `GHOSTCAM_HTTP_PORT` (default `ghostcam::config::HTTP_PORT`)
- `GHOSTCAM_QUIC_PORT` (default `ghostcam::config::QUIC_PORT`)
- `GHOSTCAM_REDIS_URL` (optional; empty/unset disables Redis features)

## First-run behavior
On first initialization, the process prints an initial operator password and warns to back up `ca.key`. Losing the CA private key requires re-enrollment of all cameras.

## Storage layout
Inside `GHOSTCAM_DATA_DIR`:
- `ghostcam.db` (SQLite)
- `ca.crt`, `ca.key`
- `server.crt`, `server.key`
- runtime artifacts managed by camera/API flows

## Database implementation
`src/db.rs` implements `server_core::db::Database` against SQLite:
- camera CRUD,
- enrollment token claims,
- session storage,
- API token storage,
- owner password verification/change,
- HMAC secret persistence.

User-management trait methods intentionally return errors in solo mode.
