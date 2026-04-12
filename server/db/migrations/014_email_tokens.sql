-- Migration 014: email tokens.
--
-- Single table for all one-time email secrets: verification links,
-- password-reset links, email-change confirmations, and login OTP codes.
-- The `purpose` column distinguishes them; the `token_hash` stores an
-- HMAC-SHA256 of the raw token/code (same pattern as api_tokens).

CREATE TABLE IF NOT EXISTS email_tokens (
    token_hash   TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    purpose      TEXT NOT NULL,
    payload      TEXT,
    attempts     INT NOT NULL DEFAULT 0,
    created_at   BIGINT NOT NULL,
    expires_at   BIGINT NOT NULL,
    used_at      BIGINT
);

CREATE INDEX IF NOT EXISTS idx_email_tokens_user_purpose
    ON email_tokens (user_id, purpose) WHERE used_at IS NULL;
