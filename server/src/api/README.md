# server-core/src/api
Axum HTTP layer for authentication, camera management, watch session control, telemetry history, SSE, and HLS playback access.

## Route topology
`routes.rs` splits routes into:
- **public** (no middleware),
- **protected** (wrapped by `auth_middleware`).

### Public routes
- `GET /healthz`
- `GET /readyz`
- `POST /api/v1/auth/login`

### Protected routes
- Cameras:
  - `GET /api/v1/cameras`
  - `POST /api/v1/cameras`
  - `GET /api/v1/cameras/:device_id`
  - `PATCH /api/v1/cameras/:device_id`
  - `DELETE /api/v1/cameras/:device_id`
- Watch/session:
  - `POST /api/v1/watch`
  - `DELETE /api/v1/session/:id`
  - `POST /api/v1/session/:id/ice` (currently no-op for ICE-lite compatibility)
- API tokens:
  - `GET /api/v1/tokens`
  - `POST /api/v1/tokens`
  - `DELETE /api/v1/tokens/:token_id`
- Telemetry:
  - `GET /api/v1/telemetry/:device_id/latest`
  - `GET /api/v1/telemetry/:device_id?from=&to=&cursor=&limit=`
- SSE:
  - `GET /events`
- HLS:
  - `GET /hls/:device_id/init.mp4`
  - `GET /hls/:device_id/playlist.m3u8`
  - `GET /hls/:device_id/:segment_id`
- Auth maintenance:
  - `POST /api/v1/auth/logout`
  - `PATCH /api/v1/auth/password`

## Auth model
`auth_middleware` accepts either:
1. `Authorization: Bearer <token>` (HMAC-verified API token), or
2. `ghostcam-session` cookie (DB-backed session with TTL extension).

On success it injects `AuthUser { user_id }` into request extensions.

## Handler ownership checks
All camera/session-sensitive handlers gate access by ownership:
- camera fetch/update/delete verifies camera `user_id`,
- watch creation requires camera ownership and online status,
- session teardown checks session owner.

## Readiness semantics
`/readyz` reports:
- DB health (required),
- Redis connectivity (optional: `ok` / `unavailable` / `not_configured`),
- static `quic: ok` marker (process-level availability assumption).
