-- Ed25519 public key storage for signature-based camera authentication.
-- Replaces HMAC API key auth; both auth methods coexist during migration.

CREATE TABLE IF NOT EXISTS camera_public_keys (
    device_id   TEXT PRIMARY KEY REFERENCES cameras(device_id) ON DELETE CASCADE,
    public_key  TEXT NOT NULL,  -- hex-encoded ed25519 public key (64 chars)
    created_at  BIGINT NOT NULL,
    updated_at  BIGINT NOT NULL
);
