# Ghostcam — Database and Application State

**Status:** Draft

---

## 1. Overview

This document specifies the persistent application state for Ghostcam: the data models for users and cameras, the database technology chosen for each implementation variant, the relationship between application state and the Redis telemetry store, and the implications for server-side routing.

Ghostcam ships as two implementation variants in the same Rust workspace:

- **`server-solo`** — single-owner, self-hostable, open source. One password-protected operator account, no registration flow, no user table.
- **`server-multi`** — multi-user, hosted by Ghostcam. Multiple independent user accounts, each owning a disjoint set of cameras.

The camera firmware and wire protocol are identical for both. The shared `server-core` crate contains all ingest, egress, and routing logic. The two server binaries differ in their auth middleware, HTTP API surface, and database layer.

Redis remains the store for telemetry history and segment metadata in both variants (see `telemetry.md` and `playback.md`). The application database described here covers entities that Redis is not suited for: structured records with relational queries, credentials, and camera ownership.

---

## 2. Design Principles

**Cameras belong directly to users.** There are no groups. A camera is owned by a user (or, in `server-solo`, by the single operator). The viewer sees all cameras belonging to the authenticated principal. Routing on the server is scoped by owner identity, not by a named group.

**The database is the authoritative camera registry.** The server has a persistent record of enrolled cameras and can distinguish an unknown device from a known-but-offline one. A camera with no database record is rejected at the QUIC ingest regardless of certificate validity.

**Auth is performed at the HTTP layer, not only at the QUIC/mTLS layer.** The mTLS device and association certificates (specified in `auth.md`) authenticate the camera to the QUIC ingest. The application database authenticates human users to the HTTP API and the WebRTC signaling surface.

**In-memory routing state remains soft.** IngestSlots, EgressHandles, and the routing registry are reconstructed from camera reconnections. The database is not involved in the media path.

---

## 3. Workspace Structure

```
ghostcam/
├── ghostcam/          # shared types, wire protocol, codec helpers
├── camera/            # firmware — unchanged
├── server-core/       # shared ingest, egress, routing, Redis integration
├── server-solo/       # single-user binary (SQLite)
└── server-multi/      # multi-user binary (Postgres)
```

`server-core` exposes a `Database` trait (or equivalent Rust abstraction) that both binaries implement. Handlers in `server-core` call into this trait for camera lookups and auth verification; the concrete implementation is injected at startup.

---

## 4. `server-solo` — Single-Owner, SQLite

### 4.1 Technology

**SQLite** via `sqlx` (with the `sqlite` feature and compile-time query checking). The database file defaults to `ghostcam.db` in the working directory, configurable via `--db-path` or `GHOSTCAM_DB_PATH`. No separate database process is required. The file should be placed on a persistent volume when running in Docker.

Migrations are managed by `sqlx-cli` and embedded in the binary via `sqlx::migrate!()`, running automatically at startup.

### 4.2 Authentication

`server-solo` has a single operator account protected by a password. There is no registration flow. On first startup, if no password has been set, the server generates a random initial password, prints it once to stdout, and requires it to be changed on first login. After login the server issues a session token (stored in the `sessions` table) returned as an `HttpOnly` cookie.

The operator can also create long-lived API tokens (stored in `api_tokens`) for programmatic access and integrations. The password and session cookie are used by the browser UI; API tokens are used by scripts and third-party integrations.

### 4.3 Data Model

#### `owner`

A single-row table holding the operator's credentials. The application enforces that this table never has more than one row.

| Column | Type | Notes |
|--------|------|-------|
| `id` | `INTEGER PRIMARY KEY` | Always 1 |
| `password_hash` | `TEXT NOT NULL` | Argon2id hash of the operator password |
| `display_name` | `TEXT NOT NULL` | Shown in the UI header |
| `password_changed_at` | `INTEGER NOT NULL` | Unix timestamp; used to detect whether the initial password has been changed |

#### `sessions`

| Column | Type | Notes |
|--------|------|-------|
| `session_id` | `TEXT PRIMARY KEY` | Cryptographically random, URL-safe base64, 32 bytes |
| `created_at` | `INTEGER NOT NULL` | Unix timestamp |
| `expires_at` | `INTEGER NOT NULL` | Default 30 days, extended on activity |
| `last_active_at` | `INTEGER` | |
| `user_agent` | `TEXT` | For session management UI |

#### `cameras`

The single source of truth for enrolled cameras. A camera must have a row here before the server will accept its QUIC connection. The `device_id` is a server-generated UUID assigned at enrollment time. The camera never stores or transmits its `device_id` — the server derives camera identity from the device certificate fingerprint.

| Column | Type | Notes |
|--------|------|-------|
| `device_id` | `TEXT PRIMARY KEY` | Server-generated UUID, assigned at enrollment |
| `cert_fingerprint` | `TEXT UNIQUE NOT NULL` | SHA-256 fingerprint of the device identity certificate public key. Used to look up the camera on each QUIC connection. |
| `display_name` | `TEXT NOT NULL` | Human-readable label, editable in the UI. Defaults to "New Camera" if not provided at enrollment. |
| `enrolled_at` | `INTEGER NOT NULL` | Unix timestamp (seconds) |
| `last_seen_at` | `INTEGER` | Updated on each successful QUIC connect; NULL if never connected post-enrollment |
| `notes` | `TEXT` | Optional freeform notes |

All cameras belong to the single operator implicitly; there is no `user_id` column. On unregistration the row is hard-deleted along with all associated Redis telemetry and segment metadata.

#### `enrollment_tokens`

Records for enrollment JWTs in flight. The JWT `jti` claim is stored here for replay prevention — a JWT may be claimed at most once. The JWT payload (server address, display name, WiFi credentials) is carried in the JWT itself, not in this table.

| Column | Type | Notes |
|--------|------|-------|
| `jti` | `TEXT PRIMARY KEY` | UUID matching the `jti` claim in the enrollment JWT |
| `expires_at` | `INTEGER NOT NULL` | Unix timestamp; matches `exp` in the JWT |
| `claimed_by` | `TEXT` | `device_id` of the camera that claimed it; NULL if unclaimed |
| `claimed_at` | `INTEGER` | Unix timestamp of claim; NULL if unclaimed |

Expired unclaimed tokens are deleted on startup and periodically during operation.

#### `api_tokens`

Long-lived bearer tokens for programmatic access and integrations. Separate from session cookies.

The `server_secret` used in HMAC computation is a 32-byte random value generated on first startup and persisted in the database alongside the `owner` record (or in a dedicated `config` key-value row). It never changes after generation. Rotating it invalidates all existing API tokens.

| Column | Type | Notes |
|--------|------|-------|
| `token_id` | `TEXT PRIMARY KEY` | Random identifier, safe to expose in management UI |
| `token_hash` | `TEXT UNIQUE NOT NULL` | `HMAC-SHA256(raw_token, server_secret)`, hex-encoded. Raw token shown once on creation and never stored. Verification is O(1) with a constant-time compare. |
| `label` | `TEXT NOT NULL` | Human-readable description (e.g. "Home Assistant", "CLI script") |
| `created_at` | `INTEGER NOT NULL` | Unix timestamp |
| `expires_at` | `INTEGER` | NULL means non-expiring |
| `last_used_at` | `INTEGER` | Updated on each successful auth |

---

## 5. `server-multi` — Multi-User, Postgres

### 5.1 Technology

**Postgres** via `sqlx` (with the `postgres` feature). Connection string configured via `DATABASE_URL`. Connection pooling via `sqlx::PgPool` with a configurable pool size.

Migrations are managed by `sqlx-cli` and embedded in the binary, running automatically at startup behind a Postgres advisory lock to prevent concurrent migration runs in horizontally-scaled deployments.

### 5.2 Data Model

#### `users`

| Column | Type | Notes |
|--------|------|-------|
| `user_id` | `UUID PRIMARY KEY` | Generated server-side (`gen_random_uuid()`) |
| `email` | `TEXT UNIQUE NOT NULL` | Login identifier; normalised to lowercase |
| `password_hash` | `TEXT NOT NULL` | Argon2id hash |
| `display_name` | `TEXT NOT NULL` | Shown in the UI |
| `created_at` | `TIMESTAMPTZ NOT NULL` | |
| `verified_at` | `TIMESTAMPTZ` | NULL until email is verified |
| `disabled_at` | `TIMESTAMPTZ` | NULL unless account is suspended |

#### `cameras`

| Column | Type | Notes |
|--------|------|-------|
| `device_id` | `UUID PRIMARY KEY` | Server-generated UUID, assigned at enrollment |
| `user_id` | `UUID NOT NULL REFERENCES users(user_id)` | Owning user |
| `cert_fingerprint` | `TEXT UNIQUE NOT NULL` | SHA-256 fingerprint of device identity certificate public key |
| `display_name` | `TEXT NOT NULL` | Defaults to "New Camera" if not provided at enrollment |
| `enrolled_at` | `TIMESTAMPTZ NOT NULL` | |
| `last_seen_at` | `TIMESTAMPTZ` | |
| `notes` | `TEXT` | |

Index on `user_id` — the primary query pattern is "all cameras owned by user X". On unregistration the row is hard-deleted along with all associated Redis data.

#### `enrollment_tokens`

| Column | Type | Notes |
|--------|------|-------|
| `jti` | `UUID PRIMARY KEY` | Matches the `jti` claim in the enrollment JWT |
| `user_id` | `UUID NOT NULL REFERENCES users(user_id)` | The user initiating enrollment |
| `expires_at` | `TIMESTAMPTZ NOT NULL` | Matches `exp` in the JWT |
| `claimed_by` | `UUID REFERENCES cameras(device_id)` | NULL if unclaimed |
| `claimed_at` | `TIMESTAMPTZ` | |

#### `sessions`

HTTP session tokens issued after a successful password login. Short-lived; not used for API integrations.

| Column | Type | Notes |
|--------|------|-------|
| `session_id` | `UUID PRIMARY KEY` | |
| `user_id` | `UUID NOT NULL REFERENCES users(user_id)` | |
| `created_at` | `TIMESTAMPTZ NOT NULL` | |
| `expires_at` | `TIMESTAMPTZ NOT NULL` | Default 30 days; extended on activity |
| `last_active_at` | `TIMESTAMPTZ` | |
| `user_agent` | `TEXT` | For session management UI |
| `ip_address` | `INET` | |

#### `api_tokens`

Long-lived tokens for programmatic access (integrations, scripts). Separate from session cookies.

| Column | Type | Notes |
|--------|------|-------|
| `token_id` | `UUID PRIMARY KEY` | |
| `user_id` | `UUID NOT NULL REFERENCES users(user_id)` | |
| `token_hash` | `TEXT UNIQUE NOT NULL` | `HMAC-SHA256(raw_token, server_secret)`, hex-encoded. Same scheme as `server-solo`. |
| `label` | `TEXT NOT NULL` | |
| `created_at` | `TIMESTAMPTZ NOT NULL` | |
| `expires_at` | `TIMESTAMPTZ` | |
| `last_used_at` | `TIMESTAMPTZ` | |

---

## 6. Implications for Other Specs

### 6.1 `DeviceHello` and the wire protocol

`DeviceHello` carries only `device_id` and `capabilities`. There is no `group_id` field. After the TLS handshake the server looks up `device_id` in the database to determine the owning user and whether the device is enrolled. See `wire-protocol.md`.

### 6.2 Routing registry

The server-side routing registry maps `owner_identity → [IngestSlot]` rather than any group concept. In `server-solo` the identity is trivially the single operator; in `server-multi` it is `user_id`. A WebRTC session is scoped to an owner identity: the observer sees all cameras belonging to that identity that are currently connected. See `ingest.md`.

### 6.3 Camera acceptance at QUIC ingest

After the mTLS handshake passes, the server performs a database lookup on `device_id` before admitting the connection to an IngestSlot. If the `device_id` is not found in `cameras`, the connection is rejected with a QUIC application error. A new error code for "device not enrolled" is required in `wire-protocol.md`. See `ingest.md` §3.

### 6.4 Enrollment flow

The enrollment token table resolves the open question in `auth.md` §4 about how the server knows which account a camera should be enrolled into. The operator (solo) or user (multi) initiates enrollment in the UI; the server creates an `enrollment_tokens` row; the QR code encodes the token and server address. The camera presents the token on its pre-enrollment QUIC connection; the server creates the `cameras` row and issues the user association certificate. See `auth.md`.

### 6.5 HTTP API — watch endpoint

`POST /api/v1/watch` carries no group path parameter. The session is scoped to the authenticated principal; the server derives which IngestSlots to include from the owner identity. See `webrtc-client.md`.

### 6.6 Telemetry and segment metadata

Redis namespacing (`telemetry:{device_id}`, `segments:{device_id}:*`) is unchanged. `device_id` is globally unique; ownership is enforced at the HTTP layer by the application database before any Redis query is issued. See `telemetry.md` and `playback.md`.

---

## 7. HTTP API

All endpoints except auth and health require a valid session cookie or `Authorization: Bearer <api_token>`.

### `server-solo`

```
POST   /api/v1/auth/login                Password login → session cookie
POST   /api/v1/auth/logout               Invalidate session
PATCH  /api/v1/auth/password             Change operator password

GET    /api/v1/cameras                   List all cameras (online and offline)
POST   /api/v1/cameras/enroll            Generate enrollment JWT → { qr_url, expires_at }
                                         Body: { display_name?: string, wifi?: [{ssid, psk}] }
GET    /api/v1/cameras/{device_id}       Camera record
PATCH  /api/v1/cameras/{device_id}       Update display_name, notes
DELETE /api/v1/cameras/{device_id}       Unenroll camera (hard delete, purges all data)

GET    /api/v1/cameras/{device_id}/networks        List known WiFi networks on camera
POST   /api/v1/cameras/{device_id}/networks        Add a network → triggers network_config command
                                                    Body: { ssid: string, psk: string }
DELETE /api/v1/cameras/{device_id}/networks/{ssid} Remove a network → triggers remove_network command

POST   /api/v1/watch                     SDP offer → { session_id, sdp_answer }
                                         Body: { sdp_offer: string, device_id: string }
DELETE /api/v1/session/{id}              Tear down WebRTC session
POST   /api/v1/session/{id}/ice          Trickle ICE candidate

GET    /api/v1/tokens                    List API tokens
POST   /api/v1/tokens                    Create API token → { token_id, raw_token } (raw shown once)
DELETE /api/v1/tokens/{token_id}         Revoke API token

GET    /healthz                          No auth
GET    /readyz                           No auth
```

### `server-multi`

Auth endpoints replace the solo password flow with a full account model:

```
POST   /api/v1/auth/register             Create account { email, password, display_name }
POST   /api/v1/auth/login                → session cookie
POST   /api/v1/auth/logout               Invalidate session
GET    /api/v1/auth/sessions             List active sessions for current user
DELETE /api/v1/auth/sessions/{id}        Revoke a session

GET    /api/v1/user                      Current user record
PATCH  /api/v1/user                      Update display_name, email, password
```

Camera, watch, and token endpoints are identical to `server-solo`, scoped to the authenticated `user_id`.

---

## 8. Open Questions

| Question | Notes |
|----------|-------|
| `server-solo` first-run UX | Generated initial password printed to stdout is workable for a CLI tool; a self-hosted Docker user may miss it. An alternative is requiring `--initial-password` at first start. |
| Email verification in `server-multi` | Is verification required before cameras can be enrolled? What is the email delivery mechanism (SMTP config, third-party)? |
| Password reset flow in `server-multi` | Requires email delivery; out of scope for initial spec but needs a placeholder. |
| Session token transport | `HttpOnly` cookie (preferred for browser) vs. bearer token in response body (easier for native/CLI). Cookies are the current assumption; needs confirmation. |
| Rate limiting on auth endpoints | Login, register, and enrollment JWT generation should be rate-limited. Mechanism (in-process token bucket, Redis, external) not yet specified. |
| QUIC rejection error code for unenrolled device | A new application-layer error code for "device not enrolled" is needed in `wire-protocol.md`. |
