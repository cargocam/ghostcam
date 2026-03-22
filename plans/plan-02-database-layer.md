# Plan 2: Database Layer

**Status:** Not started
**Branch:** `rewrite`
**Depends on:** Plan 1 (shared types, Database trait)
**Unlocks:** Plan 3 (ingest pipeline), Plan 4 (PKI & enrollment)

---

## 1. Goal

Implement the `Database` trait from `server-core` for both server variants: SQLite (`server-solo`) and Postgres (`server-multi`). Set up schema migrations, first-run initialization (operator password generation, HMAC secret generation), and Argon2id password hashing. After this plan, both server binaries can boot, run migrations, and perform all CRUD operations against their respective databases.

---

## 2. Crate Changes

### 2.1 New Dependencies

**Workspace `Cargo.toml`** — add:
```toml
[workspace.dependencies]
sqlx = { version = "0.8", features = ["runtime-tokio"] }
argon2 = "0.5"
rand = "0.8"
base64 = "0.22"
uuid = { version = "1", features = ["v4"] }
async-trait = "0.1"
```

**`server-core/Cargo.toml`** — add:
```toml
[dependencies]
ghostcam = { path = "../ghostcam" }
async-trait.workspace = true
anyhow.workspace = true
tokio.workspace = true
argon2.workspace = true
rand.workspace = true
ring.workspace = true
base64.workspace = true
uuid.workspace = true
tracing.workspace = true
```

**`server-solo/Cargo.toml`** — add:
```toml
[dependencies]
ghostcam = { path = "../ghostcam" }
server-core = { path = "../server-core" }
sqlx = { workspace = true, features = ["sqlite"] }
tokio.workspace = true
anyhow.workspace = true
tracing.workspace = true
tracing-subscriber = "0.3"
```

**`server-multi/Cargo.toml`** — add:
```toml
[dependencies]
ghostcam = { path = "../ghostcam" }
server-core = { path = "../server-core" }
sqlx = { workspace = true, features = ["postgres"] }
tokio.workspace = true
anyhow.workspace = true
tracing.workspace = true
tracing-subscriber = "0.3"
```

---

## 3. Implementation Details

### 3.1 Password & Token Utilities (`server-core/src/auth.rs`)

Shared auth utilities used by both server variants.

```rust
use argon2::{Argon2, PasswordHash, PasswordHasher, PasswordVerifier};
use argon2::password_hash::SaltString;
use rand::rngs::OsRng;

/// Hash a password with Argon2id. Returns the PHC-formatted hash string.
pub fn hash_password(password: &str) -> Result<String>;

/// Verify a password against an Argon2id PHC hash string.
pub fn verify_password(password: &str, hash: &str) -> Result<bool>;

/// Generate a cryptographically random password (16 alphanumeric chars).
pub fn generate_random_password() -> String;

/// Generate a cryptographically random session ID (32 bytes, URL-safe base64).
pub fn generate_session_id() -> SessionId;

/// Generate a cryptographically random API token (32 bytes, URL-safe base64).
/// Returns (token_id, raw_token) — raw_token is shown once and never stored.
pub fn generate_api_token() -> (TokenId, String);

/// Compute HMAC-SHA256 of a raw API token using the server secret.
/// Returns hex-encoded hash for storage and comparison.
pub fn hmac_token(raw_token: &str, secret: &[u8]) -> String;

/// Constant-time comparison of two hex-encoded HMAC values.
pub fn verify_token_hmac(raw_token: &str, stored_hash: &str, secret: &[u8]) -> bool;

/// Generate a 32-byte random HMAC secret.
pub fn generate_hmac_secret() -> Vec<u8>;
```

### 3.2 Database Trait Update (`server-core/src/db.rs`)

Add `async-trait` attribute and a `user_id_for_solo()` helper:

```rust
use async_trait::async_trait;

/// The fixed UserId for server-solo (single operator, always ID "1").
pub const SOLO_USER_ID: &str = "solo";

#[async_trait]
pub trait Database: Send + Sync + 'static {
    // ... (all methods from Plan 1)

    /// Run database migrations. Called once at startup.
    async fn migrate(&self) -> Result<()>;

    /// Check if the database is reachable (for health checks).
    async fn health_check(&self) -> Result<()>;
}
```

### 3.3 SQLite Schema (`server-solo/migrations/`)

#### `001_initial.sql`

```sql
CREATE TABLE IF NOT EXISTS config (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS owner (
    id                  INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash       TEXT NOT NULL,
    display_name        TEXT NOT NULL DEFAULT 'Operator',
    password_changed_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    session_id     TEXT PRIMARY KEY,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    last_active_at INTEGER,
    user_agent     TEXT
);

CREATE TABLE IF NOT EXISTS cameras (
    device_id        TEXT PRIMARY KEY,
    cert_fingerprint TEXT UNIQUE NOT NULL,
    display_name     TEXT NOT NULL DEFAULT 'New Camera',
    enrolled_at      INTEGER NOT NULL,
    last_seen_at     INTEGER,
    notes            TEXT
);

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    jti        TEXT PRIMARY KEY,
    expires_at INTEGER NOT NULL,
    claimed_by TEXT REFERENCES cameras(device_id),
    claimed_at INTEGER
);

CREATE TABLE IF NOT EXISTS api_tokens (
    token_id     TEXT PRIMARY KEY,
    token_hash   TEXT UNIQUE NOT NULL,
    label        TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    last_used_at INTEGER
);
```

### 3.4 SQLite Implementation (`server-solo/src/db.rs`)

```rust
pub struct SqliteDatabase {
    pool: SqlitePool,
}

impl SqliteDatabase {
    /// Open (or create) the SQLite database at the given path.
    /// Runs migrations automatically.
    pub async fn open(path: &str) -> Result<Self>;

    /// First-run initialization:
    /// 1. Check if `owner` row exists
    /// 2. If not: generate random password, hash with Argon2id, insert owner row
    /// 3. Check if `config` has `hmac_secret` key
    /// 4. If not: generate 32-byte random secret, insert
    /// 5. Return the initial password (if generated) so the caller can print it
    pub async fn initialize(&self) -> Result<Option<String>>;
}
```

Key implementation notes:
- All `user_id` parameters are ignored in server-solo (single operator) — the impl uses the fixed `SOLO_USER_ID` internally
- `verify_password` loads the owner row and calls `auth::verify_password`
- `set_password` hashes with Argon2id and updates the owner row + `password_changed_at`
- `get_hmac_secret` reads from the `config` table
- `create_camera` generates a UUID `device_id` via `uuid::Uuid::new_v4()`
- `claim_enrollment_token` is atomic: checks unclaimed + not expired, sets `claimed_by` and `claimed_at` in one UPDATE with a WHERE clause, returns false if 0 rows affected
- `cleanup_expired_tokens` deletes unclaimed tokens where `expires_at < now`
- `cleanup_expired_sessions` deletes sessions where `expires_at < now`
- All timestamps are Unix seconds (not milliseconds) for SQLite INTEGER columns, matching the spec

### 3.5 Postgres Schema (`server-multi/migrations/`)

#### `001_initial.sql`

```sql
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE IF NOT EXISTS config (
    key   TEXT PRIMARY KEY,
    value BYTEA NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    user_id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    display_name  TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_at   TIMESTAMPTZ,
    disabled_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS sessions (
    session_id     UUID PRIMARY KEY,
    user_id        UUID NOT NULL REFERENCES users(user_id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,
    last_active_at TIMESTAMPTZ,
    user_agent     TEXT,
    ip_address     INET
);

CREATE TABLE IF NOT EXISTS cameras (
    device_id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          UUID NOT NULL REFERENCES users(user_id),
    cert_fingerprint TEXT UNIQUE NOT NULL,
    display_name     TEXT NOT NULL DEFAULT 'New Camera',
    enrolled_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at     TIMESTAMPTZ,
    notes            TEXT
);

CREATE INDEX idx_cameras_user_id ON cameras(user_id);

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    jti        UUID PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users(user_id),
    expires_at TIMESTAMPTZ NOT NULL,
    claimed_by UUID REFERENCES cameras(device_id),
    claimed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS api_tokens (
    token_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(user_id),
    token_hash   TEXT UNIQUE NOT NULL,
    label        TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);
```

### 3.6 Postgres Implementation (`server-multi/src/db.rs`)

```rust
pub struct PostgresDatabase {
    pool: PgPool,
}

impl PostgresDatabase {
    /// Connect to Postgres using the provided connection string.
    /// Runs migrations behind a Postgres advisory lock.
    pub async fn connect(database_url: &str) -> Result<Self>;

    /// Initialize the HMAC secret in the config table if not present.
    /// No owner/password initialization — server-multi uses user registration.
    pub async fn initialize(&self) -> Result<()>;
}
```

Key differences from SQLite:
- `user_id` is always required and meaningful — cameras are scoped to users
- `list_cameras` filters by `user_id`
- `verify_password` looks up by email in the `users` table, returns the associated `user_id`
- Sessions carry `user_id` and optional `ip_address`
- Enrollment tokens carry `user_id`
- API tokens carry `user_id`
- Migration runs behind `SELECT pg_advisory_lock(hash)` to prevent concurrent migration in multi-instance deploys
- Timestamps use `TIMESTAMPTZ` (Postgres) vs `INTEGER` (SQLite) — the trait layer normalizes to `u64` Unix timestamps

### 3.7 Database Trait Additional Methods

Add user management methods needed by `server-multi` only. These return `Err` with "not supported" in the `server-solo` implementation:

```rust
#[async_trait]
pub trait Database: Send + Sync + 'static {
    // ... existing methods ...

    // --- User management (server-multi only) ---
    async fn create_user(&self, email: &str, password_hash: &str, display_name: &str) -> Result<UserId>;
    async fn get_user_by_email(&self, email: &str) -> Result<Option<UserRecord>>;
    async fn get_user(&self, user_id: &UserId) -> Result<Option<UserRecord>>;
    async fn update_user(&self, user_id: &UserId, update: &UserUpdate) -> Result<()>;
}

pub struct UserRecord {
    pub user_id: UserId,
    pub email: String,
    pub display_name: String,
    pub created_at: u64,
    pub verified_at: Option<u64>,
    pub disabled_at: Option<u64>,
}

pub struct UserUpdate {
    pub email: Option<String>,
    pub display_name: Option<String>,
    pub password_hash: Option<String>,
}
```

### 3.8 Server Binary Stubs Update

**`server-solo/src/main.rs`** — update from print stub to actual boot:

```rust
#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::init();

    let data_dir = std::env::var("GHOSTCAM_DATA_DIR")
        .unwrap_or_else(|_| "/var/ghostcam".to_string());
    let db_path = format!("{}/ghostcam.db", data_dir);

    let db = SqliteDatabase::open(&db_path).await?;
    if let Some(initial_password) = db.initialize().await? {
        println!("============================================================");
        println!("Ghostcam server-solo first run");
        println!();
        println!("Initial operator password: {}", initial_password);
        println!();
        println!("Log in and change this password.");
        println!();
        println!("IMPORTANT: Back up {}/ca.key", data_dir);
        println!("Losing this file requires re-enrolling all cameras.");
        println!("============================================================");
    }

    tracing::info!("database initialized at {}", db_path);

    // Server logic will be added in later plans
    Ok(())
}
```

**`server-multi/src/main.rs`** — similar but with Postgres:

```rust
#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::init();

    let database_url = std::env::var("DATABASE_URL")
        .expect("DATABASE_URL must be set");

    let db = PostgresDatabase::connect(&database_url).await?;
    db.initialize().await?;

    tracing::info!("database initialized");

    // Server logic will be added in later plans
    Ok(())
}
```

---

## 4. Testing Plan

### 4.1 Unit Tests — Auth Utilities

**Location:** `server-core/src/auth.rs` test module

| Test | Description |
|------|-------------|
| `hash_and_verify_password` | Hash "password123", verify against same string → true |
| `verify_wrong_password` | Hash "password123", verify against "wrong" → false |
| `hash_is_argon2id` | Hash a password, verify the PHC string starts with `$argon2id$` |
| `hash_is_unique` | Hash same password twice, verify hashes differ (random salt) |
| `generate_random_password_length` | Generated password is 16 characters |
| `generate_random_password_alphanumeric` | Generated password contains only alphanumeric chars |
| `generate_random_password_unique` | Two calls produce different passwords |
| `generate_session_id_length` | Session ID base64 decodes to 32 bytes |
| `generate_session_id_unique` | Two calls produce different IDs |
| `generate_api_token_unique` | Two calls produce different (token_id, raw_token) pairs |
| `hmac_token_deterministic` | Same token + same secret → same hash |
| `hmac_token_different_secret` | Same token + different secret → different hash |
| `verify_token_hmac_valid` | Generate token, compute HMAC, verify → true |
| `verify_token_hmac_invalid` | Verify wrong token against stored hash → false |
| `verify_token_hmac_timing_safe` | Verify that comparison uses constant-time equality (structural — verify `ring::constant_time` or equivalent is used) |
| `generate_hmac_secret_length` | Secret is 32 bytes |
| `generate_hmac_secret_unique` | Two calls produce different secrets |

### 4.2 Integration Tests — SQLite Database

**Location:** `server-solo/tests/db_tests.rs`

All tests use an in-memory SQLite database (`:memory:`) for speed and isolation. Each test creates a fresh `SqliteDatabase` instance.

#### Initialization

| Test | Description |
|------|-------------|
| `first_run_creates_owner` | Open fresh DB, call `initialize()`, verify owner row exists with hashed password |
| `first_run_returns_initial_password` | `initialize()` returns `Some(password)` on first run |
| `second_run_skips_init` | Call `initialize()` twice, second call returns `None` |
| `first_run_creates_hmac_secret` | After `initialize()`, `get_hmac_secret()` returns a 32-byte secret |
| `hmac_secret_persists` | After `initialize()`, reopen DB (same path), verify same HMAC secret returned |
| `health_check_succeeds` | `health_check()` returns `Ok(())` on a valid DB |
| `migrate_is_idempotent` | Call `migrate()` twice, no error |

#### Camera CRUD

| Test | Description |
|------|-------------|
| `create_camera_assigns_uuid` | Create camera, verify `device_id` is a valid UUID |
| `create_camera_stores_fingerprint` | Create camera, retrieve by fingerprint, verify match |
| `create_camera_default_display_name` | Create camera with "New Camera", verify stored |
| `get_camera_by_fingerprint_found` | Create camera, look up by fingerprint → `Some` |
| `get_camera_by_fingerprint_not_found` | Look up unknown fingerprint → `None` |
| `get_camera_by_id` | Create camera, look up by device_id → `Some` |
| `get_camera_by_id_not_found` | Look up unknown device_id → `None` |
| `list_cameras_empty` | No cameras enrolled → empty vec |
| `list_cameras_returns_all` | Enroll 3 cameras, list → 3 records |
| `update_camera_display_name` | Create camera, update display_name, verify |
| `update_camera_notes` | Create camera, update notes, verify |
| `update_camera_partial` | Update only display_name (notes=None), verify notes unchanged |
| `delete_camera` | Create camera, delete, verify `get_camera` returns `None` |
| `delete_camera_not_found` | Delete unknown device_id → no error (idempotent) |
| `update_last_seen` | Create camera, call `update_last_seen`, verify `last_seen_at` is set |
| `duplicate_fingerprint_rejected` | Create two cameras with same fingerprint → error |

#### Enrollment Tokens

| Test | Description |
|------|-------------|
| `create_and_claim_token` | Create token, claim it → `true` |
| `claim_token_twice_fails` | Create token, claim once → `true`, claim again → `false` |
| `claim_expired_token_fails` | Create token with `expires_at` in the past, claim → `false` |
| `claim_nonexistent_token` | Claim unknown jti → `false` |
| `cleanup_expired_tokens` | Create 2 expired + 1 valid token, cleanup → returns 2, valid token survives |
| `cleanup_preserves_claimed` | Create expired but claimed token, cleanup → not deleted (it's claimed) |

#### Sessions

| Test | Description |
|------|-------------|
| `create_and_get_session` | Create session, get by ID → matches |
| `get_session_not_found` | Get unknown session ID → `None` |
| `delete_session` | Create session, delete, get → `None` |
| `extend_session` | Create session, extend, verify `last_active_at` updated |
| `cleanup_expired_sessions` | Create 2 expired + 1 valid, cleanup → returns 2 |
| `session_user_agent` | Create session with user_agent, verify stored |

#### API Tokens

| Test | Description |
|------|-------------|
| `create_and_verify_api_token` | Create token with HMAC hash, verify with raw token → `Some` |
| `verify_wrong_token` | Verify wrong raw token → `None` |
| `list_api_tokens_empty` | No tokens → empty vec |
| `list_api_tokens` | Create 3 tokens, list → 3 records |
| `delete_api_token` | Create token, delete, verify gone from list |
| `api_token_label` | Create token with label "Home Assistant", verify label in record |
| `api_token_expiry` | Create token with `expires_at`, verify stored |

#### Password Management

| Test | Description |
|------|-------------|
| `verify_initial_password` | After `initialize()`, verify the returned password works |
| `set_password_and_verify` | Set new password, verify old fails, new succeeds |
| `set_password_updates_changed_at` | Set password, verify `password_changed_at` updated |

### 4.3 Integration Tests — Postgres Database

**Location:** `server-multi/tests/db_tests.rs`

These tests require a running Postgres instance. They are gated behind a `#[cfg(feature = "postgres-tests")]` feature flag (or `#[ignore]` with `-- --ignored` to run).

Each test creates a unique database (random name) to ensure isolation, and drops it on teardown.

#### Initialization

| Test | Description |
|------|-------------|
| `connect_and_migrate` | Connect, migrate → no error |
| `migrate_is_idempotent` | Migrate twice → no error |
| `initialize_creates_hmac_secret` | After `initialize()`, `get_hmac_secret()` returns 32 bytes |
| `health_check_succeeds` | `health_check()` returns `Ok(())` |

#### User Management

| Test | Description |
|------|-------------|
| `create_user` | Create user, verify UUID returned |
| `create_user_duplicate_email` | Create two users with same email → error |
| `get_user_by_email` | Create user, look up by email → `Some` |
| `get_user_by_email_case_insensitive` | Create with "User@Example.com", look up with "user@example.com" → `Some` |
| `get_user_by_id` | Create user, look up by user_id → `Some` |
| `update_user_display_name` | Update display_name, verify |
| `update_user_email` | Update email, verify |

#### Camera CRUD (scoped to user)

| Test | Description |
|------|-------------|
| `create_camera_for_user` | Create user, create camera with user_id, verify |
| `list_cameras_scoped_to_user` | Create 2 users, each with 2 cameras, list for user A → 2 cameras (not user B's) |
| `get_camera_by_fingerprint` | Create camera, look up by fingerprint → `Some` with correct user_id |
| `delete_camera_cascades` | Delete camera, verify associated enrollment tokens have NULL `claimed_by` |

#### Enrollment Tokens (scoped to user)

| Test | Description |
|------|-------------|
| `create_token_for_user` | Create user, create enrollment token with user_id |
| `claim_token` | Create and claim → `true` |
| `claim_token_twice_fails` | Claim twice → second returns `false` |

#### Sessions (scoped to user)

| Test | Description |
|------|-------------|
| `create_session_for_user` | Create user, create session with user_id |
| `session_stores_ip_address` | Create session with ip_address, verify stored |

#### API Tokens (scoped to user)

| Test | Description |
|------|-------------|
| `create_token_for_user` | Create user, create API token with user_id |
| `list_tokens_scoped_to_user` | Create 2 users, each with tokens, list for user A → only user A's tokens |

#### Password Management

| Test | Description |
|------|-------------|
| `verify_password_by_email` | Create user with hashed password, verify by email → true |
| `set_password` | Set new password, verify old fails, new succeeds |

### 4.4 Build Validation

| Check | Command | Expected |
|-------|---------|----------|
| Workspace compiles | `cargo build` | All crates compile |
| All unit tests pass | `cargo test -p server-core` | Auth utility tests pass |
| SQLite integration tests | `cargo test -p server-solo` | All SQLite tests pass |
| Postgres integration tests | `cargo test -p server-multi -- --ignored` | All Postgres tests pass (requires running Postgres) |
| server-solo boots | `GHOSTCAM_DATA_DIR=/tmp/ghostcam-test cargo run -p server-solo` | Prints initial password, creates DB file, exits |
| server-solo second boot | Run again with same data dir | Does NOT print initial password, exits cleanly |
| Clippy clean | `cargo clippy -- -D warnings` | No warnings |
| Format check | `cargo fmt --check` | Clean |

---

## 5. Validation Checklist

After completing this plan, verify:

- [ ] `server-solo/migrations/001_initial.sql` creates all 5 tables
- [ ] `server-multi/migrations/001_initial.sql` creates all 6 tables (includes `users`)
- [ ] `cargo build` succeeds for all crates
- [ ] `cargo test -p server-core` — all auth utility tests pass
- [ ] `cargo test -p server-solo` — all SQLite integration tests pass
- [ ] `server-solo` binary boots, creates DB at configurable path, prints initial password on first run
- [ ] `server-solo` binary second run does NOT regenerate password
- [ ] HMAC secret persists across restarts
- [ ] Argon2id hashes are PHC-formatted (`$argon2id$...`)
- [ ] Session IDs are 32 bytes of randomness (URL-safe base64)
- [ ] API token HMAC uses constant-time comparison
- [ ] Camera `device_id` is a UUID v4
- [ ] Enrollment token claim is atomic (no race condition on double-claim)
- [ ] SQLite `owner` table enforces single-row constraint (`CHECK (id = 1)`)
- [ ] Postgres `cameras` table has index on `user_id`
- [ ] Postgres email is normalized to lowercase
- [ ] Postgres migrations run behind advisory lock
- [ ] `Database` trait is fully implemented for both `SqliteDatabase` and `PostgresDatabase`
- [ ] All methods that are server-multi-only return appropriate errors in server-solo impl
