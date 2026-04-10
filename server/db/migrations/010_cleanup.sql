-- Drop the sessions table: auth is now fully stateless (JWT cookies + API tokens).
DROP TABLE IF EXISTS sessions;

-- Drop the owner table: superseded by users in migration 002.
DROP TABLE IF EXISTS owner;

-- Drop cert_fingerprint: cameras authenticate via API keys (see 007_hls_rewrite.sql).
ALTER TABLE cameras DROP COLUMN IF EXISTS cert_fingerprint;

-- Drop enrollment_tokens: superseded by provision_tokens in migration 007.
DROP TABLE IF EXISTS enrollment_tokens;
