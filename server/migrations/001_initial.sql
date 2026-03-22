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
    user_agent     TEXT,
    ip_address     TEXT
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
