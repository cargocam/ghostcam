-- Migration 013: user soft delete.
--
-- Adds `deleted_at` to users so admin-initiated account removal is
-- reversible without touching Stripe, S3, or the cameras table. Hard
-- deletion stays available for developers via direct psql — by design,
-- per the project's "admin actions are UI-first, destructive cleanup is
-- deliberate and manual" stance.
--
-- On soft delete the admin handler also sets disabled_at so the login
-- path keeps rejecting the account through its existing gate, and
-- cameraAuth gains an owner-deleted check so segments stop flowing.

DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'users' AND column_name = 'deleted_at'
    ) THEN
        ALTER TABLE users ADD COLUMN deleted_at BIGINT;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_users_deleted_at ON users(deleted_at)
    WHERE deleted_at IS NOT NULL;
