CREATE TABLE IF NOT EXISTS config (
    key   TEXT PRIMARY KEY,
    value BYTEA NOT NULL
);

CREATE TABLE IF NOT EXISTS owner (
    id                  INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash       TEXT NOT NULL,
    display_name        TEXT NOT NULL DEFAULT 'Operator',
    password_changed_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    session_id     TEXT PRIMARY KEY,
    created_at     BIGINT NOT NULL,
    expires_at     BIGINT NOT NULL,
    last_active_at BIGINT,
    user_agent     TEXT,
    ip_address     TEXT
);

CREATE TABLE IF NOT EXISTS cameras (
    device_id        TEXT PRIMARY KEY,
    cert_fingerprint TEXT UNIQUE NOT NULL,
    display_name     TEXT NOT NULL DEFAULT 'New Camera',
    enrolled_at      BIGINT NOT NULL,
    last_seen_at     BIGINT,
    notes            TEXT
);

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    jti        TEXT PRIMARY KEY,
    expires_at BIGINT NOT NULL,
    claimed_by TEXT REFERENCES cameras(device_id),
    claimed_at BIGINT
);

CREATE TABLE IF NOT EXISTS api_tokens (
    token_id     TEXT PRIMARY KEY,
    token_hash   TEXT UNIQUE NOT NULL,
    label        TEXT NOT NULL,
    created_at   BIGINT NOT NULL,
    expires_at   BIGINT,
    last_used_at BIGINT
);
