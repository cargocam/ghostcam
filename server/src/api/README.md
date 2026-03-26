# api

Axum HTTP router. Authentication is enforced via middleware on all protected routes — either `Authorization: Bearer <token>` header or a `session=<id>` cookie.

## Routes

### Public (no auth)

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| `POST` | `/api/v1/auth/login` | `auth::login` | Password login → sets `session` cookie |
| `GET` | `/healthz` | `health::healthz` | Always `200 ok` |
| `GET` | `/readyz` | `health::readyz` | `200` when DB + QUIC listener are ready |

### Auth (protected)

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| `POST` | `/api/v1/auth/logout` | `auth::logout` | Clears session cookie, deletes server-side session |
| `PATCH` | `/api/v1/auth/password` | `auth::change_password` | Update operator password |

### Cameras

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| `GET` | `/api/v1/cameras` | `cameras::list` | All enrolled cameras with connection status |
| `POST` | `/api/v1/cameras` | `cameras::enroll` | Issue enrollment token → camera uses it to get a signed cert |
| `GET` | `/api/v1/cameras/:id` | `cameras::get` | Camera record + latest telemetry |
| `PATCH` | `/api/v1/cameras/:id` | `cameras::update` | Update display name or group |
| `DELETE` | `/api/v1/cameras/:id` | `cameras::delete` | Revoke enrollment, add fingerprint to CRL |

### WebRTC Sessions

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| `POST` | `/api/v1/watch` | `watch::create_session` | `{ device_id, sdp_offer }` → `{ session_id, sdp_answer }` |
| `DELETE` | `/api/v1/session/:id` | `watch::teardown_session` | Tear down a WebRTC session |
| `POST` | `/api/v1/session/:id/ice` | `watch::ice_candidate` | Trickle ICE candidate |

### Telemetry

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| `GET` | `/api/v1/telemetry/:id/latest` | `telemetry_api::handle_latest` | Most recent telemetry point from Redis |
| `GET` | `/api/v1/telemetry/:id` | `telemetry_api::handle_range` | Time-range query: `?from=<unix_ms>&to=<unix_ms>&limit=<n>` |

### HLS

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| `GET` | `/hls/:id/init.mp4` | `hls::get_init` | fMP4 init segment from camera ring buffer |
| `GET` | `/hls/:id/playlist.m3u8` | `hls::get_manifest` | HLS manifest |
| `GET` | `/hls/:id/:segment_id` | `hls::get_segment` | fMP4 media segment (memory or disk) |

### SSE

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| `GET` | `/events` | `sse::handle_sse` | Server-Sent Event stream for camera lifecycle and telemetry events |

### API Tokens

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| `GET` | `/api/v1/tokens` | `tokens::list` | List API tokens for current user |
| `POST` | `/api/v1/tokens` | `tokens::create` | Create a new API token |
| `DELETE` | `/api/v1/tokens/:id` | `tokens::revoke` | Revoke a token |

### Audit Log

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| `GET` | `/api/v1/audit` | `audit::query` | Query audit log entries. Params: `type` (event type filter), `since`/`until` (RFC3339 timestamps), `limit` (default 100, max 1000), `offset`. Returns `{ entries: [...], total: N }` |

## Files

| File | Purpose |
|------|---------|
| `audit.rs` | Audit log query endpoint |
| `routes.rs` | Router construction — merges protected and public sub-routers |
| `state.rs` | `AppState` — shared across all handlers: DB, Redis, PKI, session manager, SSE bus |
| `auth.rs` | Login, logout, password change, auth middleware |
| `cameras.rs` | Camera CRUD and enrollment |
| `watch.rs` | WebRTC session lifecycle |
| `hls.rs` | HLS manifest and segment serving |
| `sse.rs` | SSE handler — upgrades HTTP connection to event stream |
| `tokens.rs` | API token management |
| `health.rs` | Health and readiness probes |
| `rate_limit.rs` | Per-IP login rate limiter (5 req/min) and per-user API rate limiter (60 req/min) using `governor` |

## Rate Limiting

- **Login** (`/api/v1/auth/login`): 5 requests per 60 seconds per source IP. Returns `429 Too Many Requests` with `retry-after` header.
- **Authenticated API**: 60 requests per minute per user. Applied to all protected routes.
- **Session limit**: `POST /api/v1/watch` returns `429` when a user exceeds `MAX_SESSIONS_PER_USER` (20) active WebRTC sessions.

## Request Body Limit

All routes enforce a `MAX_REQUEST_BODY_BYTES` (1 MB) default body size limit via `DefaultBodyLimit`.
