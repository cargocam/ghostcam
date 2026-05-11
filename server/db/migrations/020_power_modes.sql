-- Power-saving modes (live / standby / sleep × proactive / lazy) plus
-- schedule + battery-rule overrides, all per-camera.
--
-- Defaults preserve every existing camera's current behaviour:
--   * power_mode = 'live'           — always-on capture and WS (today's behaviour)
--   * upload_mode = 'proactive'     — every segment uploads ASAP (today)
--   * schedule / battery_rules NULL — no overrides
--
-- segments.uploaded_to_s3 defaults TRUE so historical rows behave as
-- if they were uploaded (which they were). New segments inserted by
-- lazy-mode cameras carry uploaded_to_s3 = FALSE, signalling the
-- viewer to issue an `upload_segments` command on scrub.

ALTER TABLE cameras
    ADD COLUMN IF NOT EXISTS power_mode TEXT NOT NULL DEFAULT 'live',
    ADD COLUMN IF NOT EXISTS upload_mode TEXT NOT NULL DEFAULT 'proactive',
    ADD COLUMN IF NOT EXISTS schedule JSONB,
    ADD COLUMN IF NOT EXISTS battery_rules JSONB;

ALTER TABLE segments
    ADD COLUMN IF NOT EXISTS uploaded_to_s3 BOOLEAN NOT NULL DEFAULT TRUE;

-- Coverage queries hit (device_id, start_ts) with an optional filter on
-- uploaded_to_s3 for the lazy-mode "do I need to wake the camera to
-- fetch this segment?" check. The existing (device_id, start_ts) index
-- covers the common case; we add an explicit partial index for the
-- handful of cameras running lazy mode so the un-uploaded rows are
-- cheap to scan when a viewer scrubs.
CREATE INDEX IF NOT EXISTS idx_segments_lazy
    ON segments (device_id, start_ts)
    WHERE uploaded_to_s3 = FALSE;
