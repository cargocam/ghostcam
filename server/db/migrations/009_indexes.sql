-- Index for hourly retention cleanup (WHERE created_at < cutoff_ms).
-- Without this, DeleteOldSegments does a full table scan every hour.
CREATE INDEX IF NOT EXISTS idx_segments_created_at ON segments(created_at);
