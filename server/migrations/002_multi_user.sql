-- Migration 002: Multi-user accounts
-- Adds a users table and user_id foreign keys to all data tables.
-- Migrates the existing single owner into the first user.

-- 1. Create users table
CREATE TABLE IF NOT EXISTS users (
    user_id             TEXT PRIMARY KEY,
    email               TEXT UNIQUE NOT NULL,
    password_hash       TEXT NOT NULL,
    display_name        TEXT NOT NULL DEFAULT 'User',
    created_at          BIGINT NOT NULL,
    password_changed_at BIGINT NOT NULL,
    verified_at         BIGINT,
    disabled_at         BIGINT
);

-- 2. Migrate existing owner into users (idempotent)
INSERT INTO users (user_id, email, password_hash, display_name, created_at, password_changed_at)
SELECT
    'u-' || md5('solo-owner-migration'),
    'admin@localhost',
    o.password_hash,
    o.display_name,
    o.password_changed_at,
    o.password_changed_at
FROM owner o WHERE o.id = 1
ON CONFLICT (user_id) DO NOTHING;

-- 3. Add user_id columns (IF NOT EXISTS requires PG 9.6+)
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='cameras' AND column_name='user_id') THEN
        ALTER TABLE cameras ADD COLUMN user_id TEXT REFERENCES users(user_id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='user_id') THEN
        ALTER TABLE sessions ADD COLUMN user_id TEXT REFERENCES users(user_id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='api_tokens' AND column_name='user_id') THEN
        ALTER TABLE api_tokens ADD COLUMN user_id TEXT REFERENCES users(user_id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='enrollment_tokens' AND column_name='user_id') THEN
        ALTER TABLE enrollment_tokens ADD COLUMN user_id TEXT REFERENCES users(user_id);
    END IF;
END $$;

-- 4. Backfill existing rows to the migrated owner
-- Note: on upgrade from single-owner, the migrated user gets a deterministic
-- 'u-' prefixed ID. New users get UUID v4 IDs. Both are valid TEXT primary keys.
UPDATE cameras SET user_id = (SELECT user_id FROM users ORDER BY created_at LIMIT 1) WHERE user_id IS NULL;
UPDATE sessions SET user_id = (SELECT user_id FROM users ORDER BY created_at LIMIT 1) WHERE user_id IS NULL;
UPDATE api_tokens SET user_id = (SELECT user_id FROM users ORDER BY created_at LIMIT 1) WHERE user_id IS NULL;
UPDATE enrollment_tokens SET user_id = (SELECT user_id FROM users ORDER BY created_at LIMIT 1) WHERE user_id IS NULL;

-- 5. Make user_id NOT NULL (SET NOT NULL on already-NOT-NULL is a no-op in PostgreSQL)
ALTER TABLE cameras ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE sessions ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE api_tokens ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE enrollment_tokens ALTER COLUMN user_id SET NOT NULL;

-- 6. Indexes for user-scoped queries
CREATE INDEX IF NOT EXISTS idx_cameras_user_id ON cameras(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_api_tokens_user_id ON api_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_enrollment_tokens_user_id ON enrollment_tokens(user_id);
