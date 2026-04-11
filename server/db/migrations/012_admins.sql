-- Migration 012: Admins table.
--
-- Replaces the email-match super-admin model: admin is now any user with
-- a row in the `admins` table. Lets admins be normal users (with their
-- own subscription, billing, cameras) rather than a hardcoded account,
-- and lets admin status be granted/revoked without a token rotation.
--
-- Backfill of the existing bootstrap admin (the user whose email matched
-- GHOSTCAM_ADMIN_EMAIL pre-migration) happens in db.Initialize on the
-- next server startup — the migration does not know about env vars.

CREATE TABLE IF NOT EXISTS admins (
    user_id    TEXT PRIMARY KEY REFERENCES users(user_id) ON DELETE CASCADE,
    created_at BIGINT NOT NULL
);
