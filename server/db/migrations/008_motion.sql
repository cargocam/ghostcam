-- Add motion detection flag to segments
ALTER TABLE segments ADD COLUMN IF NOT EXISTS has_motion BOOLEAN NOT NULL DEFAULT false;
