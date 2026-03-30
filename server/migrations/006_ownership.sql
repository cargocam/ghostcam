-- Add nullable user ownership to cameras table.
-- Existing cameras already have user_id set from enrollment.
-- New cameras from auto-registration will have user_id = NULL (unclaimed).

-- Make user_id nullable (it was NOT NULL before; ALTER COLUMN handles this).
ALTER TABLE cameras ALTER COLUMN user_id DROP NOT NULL;

-- Index for efficient per-user camera listing (covers NULL for unclaimed queries).
CREATE INDEX IF NOT EXISTS idx_cameras_user_id ON cameras(user_id);
