-- Three-state segments table: pending → uploaded (or → local-only via the lazy
-- path from PR #76). The `pending` flag is set when the camera pre-announces
-- an imminent upload in its presign call's `pending` array; flipped to FALSE
-- and `uploaded_to_s3=TRUE` when the same camera's next presign confirms it
-- via the `uploaded` array (existing path).
--
-- pending_at is a wall-time timestamp used to expire stale pending rows: a
-- camera that goes silent mid-upload would otherwise leave forever-pending
-- ghosts on the timeline. Sweeper drops `pending=TRUE AND pending_at < now() -
-- 5 min` rows; UI auto-times-out the indicator without needing a corrective
-- SSE.

ALTER TABLE segments
    ADD COLUMN IF NOT EXISTS pending BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS pending_at BIGINT;

-- Coverage queries on the lazy path already use idx_segments_lazy
-- (device_id, start_ts) WHERE uploaded_to_s3=FALSE. The pending stripe
-- shares the same predicate, so the existing index already covers it —
-- no new index needed.
