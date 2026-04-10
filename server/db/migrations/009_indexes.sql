-- Index for opportunistic retention prune in the presign handler
-- (WHERE device_id = $1 AND created_at < $2). Without this, each prune
-- sub-select would do a full table scan.
CREATE INDEX IF NOT EXISTS idx_segments_created_at ON segments(created_at);
