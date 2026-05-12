package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// InsertSegments inserts a batch of segment metadata records. Segments
// inserted here are assumed UPLOADED to S3 (`uploaded_to_s3 = TRUE`) —
// the presign confirm path uses this. For lazy-mode local-only segments
// see `InsertLocalSegments`. For pre-announced segments still in flight
// see `InsertPendingSegments`.
//
// ON CONFLICT clears the `pending` flag and its timestamp so a row that
// was first registered as pending (via InsertPendingSegments) flips to
// the confirmed state cleanly. Without this clause, a confirmed row
// would carry stale pending=TRUE / pending_at metadata and the SSE
// "drop expired pending" sweeper would later issue a spurious removal.
func (db *DB) InsertSegments(ctx context.Context, segments []SegmentRecord) error {
	if len(segments) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, s := range segments {
		batch.Queue(
			`INSERT INTO segments (segment_id, device_id, s3_key, start_ts, end_ts,
			                       size_bytes, resolution, created_at, has_motion, uploaded_to_s3)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, TRUE)
			 ON CONFLICT (segment_id) DO UPDATE
			   SET uploaded_to_s3 = TRUE, pending = FALSE, pending_at = NULL`,
			s.SegmentID, s.DeviceID, s.S3Key, int64(s.StartTS), int64(s.EndTS),
			int64(s.SizeBytes), s.Resolution, int64(s.CreatedAt), s.HasMotion,
		)
	}

	br := db.pool.SendBatch(ctx, batch)
	defer br.Close()

	for range segments {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("insert segment: %w", err)
		}
	}
	return nil
}

// InsertPendingSegments records a batch of segments the camera has
// pre-announced via its presign call's `pending` array. The row carries
// `pending = TRUE` and a `pending_at` timestamp; the next presign
// confirm cycle flips it to `pending = FALSE, uploaded_to_s3 = TRUE`
// via InsertSegments' ON CONFLICT branch. Stale pending rows are
// dropped by a periodic sweeper (see PrunePendingSegments).
//
// Idempotent: ON CONFLICT DO NOTHING. The expected re-call path
// (camera retries presign because its prior request timed out) shouldn't
// reset pending_at — if the row already exists as pending we'd just
// be re-confirming the same imminent upload.
func (db *DB) InsertPendingSegments(ctx context.Context, segments []SegmentRecord, pendingAtMs uint64) error {
	if len(segments) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, s := range segments {
		batch.Queue(
			`INSERT INTO segments (segment_id, device_id, s3_key, start_ts, end_ts,
			                       size_bytes, resolution, created_at, has_motion,
			                       uploaded_to_s3, pending, pending_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, FALSE, TRUE, $10)
			 ON CONFLICT (segment_id) DO NOTHING`,
			s.SegmentID, s.DeviceID, s.S3Key, int64(s.StartTS), int64(s.EndTS),
			int64(s.SizeBytes), s.Resolution, int64(s.CreatedAt), s.HasMotion,
			int64(pendingAtMs),
		)
	}

	br := db.pool.SendBatch(ctx, batch)
	defer br.Close()

	for range segments {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("insert pending segment: %w", err)
		}
	}
	return nil
}

// PrunePendingSegments removes pending-but-never-confirmed rows that
// have aged past olderThanMs. Returns the segment_ids that were
// dropped so callers can emit a corrective SSE. The sweeper in
// server/main.go schedules this every minute with a 5-minute cutoff —
// short enough that the "blue ghost" doesn't sit on the timeline
// after a real upload failure, long enough that mild congestion
// doesn't false-positive.
func (db *DB) PrunePendingSegments(ctx context.Context, olderThanMs uint64) ([]string, error) {
	rows, err := db.pool.Query(ctx,
		`DELETE FROM segments
		 WHERE pending = TRUE AND pending_at < $1
		 RETURNING segment_id`,
		int64(olderThanMs),
	)
	if err != nil {
		return nil, fmt.Errorf("prune pending segments: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan pending segment_id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// InsertLocalSegments records lazy-mode segments the camera produced
// locally but has NOT yet uploaded to S3 (`uploaded_to_s3 = FALSE`).
// The viewer's timeline still shows them so the user knows footage
// exists; on scrub the server queues an `upload_segments` command for
// the camera so the actual bytes land in S3 on demand.
//
// ON CONFLICT preserves a row's uploaded_to_s3 state: if a segment is
// re-reported as local after it's already been uploaded (unusual but
// possible after a re-provision), we keep the uploaded_to_s3 = TRUE
// state.
func (db *DB) InsertLocalSegments(ctx context.Context, segments []SegmentRecord) error {
	if len(segments) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, s := range segments {
		batch.Queue(
			`INSERT INTO segments (segment_id, device_id, s3_key, start_ts, end_ts,
			                       size_bytes, resolution, created_at, has_motion, uploaded_to_s3)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, FALSE)
			 ON CONFLICT (segment_id) DO NOTHING`,
			s.SegmentID, s.DeviceID, s.S3Key, int64(s.StartTS), int64(s.EndTS),
			int64(s.SizeBytes), s.Resolution, int64(s.CreatedAt), s.HasMotion,
		)
	}

	br := db.pool.SendBatch(ctx, batch)
	defer br.Close()

	for range segments {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("insert local segment: %w", err)
		}
	}
	return nil
}

// ListLocalOnlySegmentIDs returns segment IDs that overlap the
// [fromTS, toTS] range and are still local-only (not yet at S3).
// Used by the scrub-driven `upload_segments` command path.
func (db *DB) ListLocalOnlySegmentIDs(
	ctx context.Context, deviceID string, fromTS, toTS uint64, limit int,
) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx,
		`SELECT segment_id
		 FROM segments
		 WHERE device_id = $1
		   AND uploaded_to_s3 = FALSE
		   AND start_ts < $3
		   AND end_ts > $2
		 ORDER BY start_ts
		 LIMIT $4`,
		deviceID, int64(fromTS), int64(toTS), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list local-only segments: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning local-only segment id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListSegments returns segments for the given device in the [fromTS, toTS] window.
// Segments older than retentionMs are excluded so that stale rows (whose S3
// objects have been reaped by the bucket lifecycle rule) never appear in the
// timeline or manifests.
func (db *DB) ListSegments(ctx context.Context, deviceID string, fromTS, toTS, retentionMs uint64) ([]SegmentRecord, error) {
	if retentionMs > 0 {
		cutoff := uint64(nowUnix())*1000 - retentionMs
		if fromTS < cutoff {
			fromTS = cutoff
		}
	}
	rows, err := db.pool.Query(ctx,
		`SELECT segment_id, device_id, s3_key, start_ts, end_ts, size_bytes, resolution, created_at, has_motion
		 FROM segments
		 WHERE device_id = $1 AND start_ts >= $2 AND start_ts <= $3
		 ORDER BY start_ts
		 LIMIT 2000`,
		deviceID, int64(fromTS), int64(toTS))
	if err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}
	defer rows.Close()

	var segments []SegmentRecord
	for rows.Next() {
		var s SegmentRecord
		var startTS, endTS, sizeBytes, createdAt int64
		if err := rows.Scan(&s.SegmentID, &s.DeviceID, &s.S3Key, &startTS, &endTS, &sizeBytes, &s.Resolution, &createdAt, &s.HasMotion); err != nil {
			return nil, fmt.Errorf("scanning segment: %w", err)
		}
		s.StartTS = uint64(startTS)
		s.EndTS = uint64(endTS)
		s.SizeBytes = uint64(sizeBytes)
		s.CreatedAt = uint64(createdAt)
		segments = append(segments, s)
	}
	return segments, rows.Err()
}

// ListSegmentCoverage returns lightweight coverage data (no s3_key, size, resolution)
// for all segments in a time range. Segments older than retentionMs are excluded
// so expired S3 objects never appear on the timeline. Capped at 50,000 rows
// (~3.5 days at 6s segments).
func (db *DB) ListSegmentCoverage(ctx context.Context, deviceID string, fromTS, toTS, retentionMs uint64) ([]CoverageRecord, error) {
	if retentionMs > 0 {
		cutoff := uint64(nowUnix())*1000 - retentionMs
		if fromTS < cutoff {
			fromTS = cutoff
		}
	}
	rows, err := db.pool.Query(ctx,
		`SELECT segment_id, start_ts, end_ts, has_motion, uploaded_to_s3
		 FROM segments
		 WHERE device_id = $1 AND start_ts >= $2 AND start_ts <= $3
		 ORDER BY start_ts
		 LIMIT 50000`,
		deviceID, int64(fromTS), int64(toTS))
	if err != nil {
		return nil, fmt.Errorf("list segment coverage: %w", err)
	}
	defer rows.Close()

	var records []CoverageRecord
	for rows.Next() {
		var r CoverageRecord
		var startTS, endTS int64
		if err := rows.Scan(&r.SegmentID, &startTS, &endTS, &r.HasMotion, &r.UploadedToS3); err != nil {
			return nil, fmt.Errorf("scanning coverage: %w", err)
		}
		r.StartTS = uint64(startTS)
		r.EndTS = uint64(endTS)
		records = append(records, r)
	}
	return records, rows.Err()
}

// PruneSegments deletes segments older than olderThanMs for the given device
// and returns the full deleted rows so the caller can reap the matching S3
// objects. Bounded by LIMIT so it is safe to call synchronously from hot
// paths. Cleanup is amortized across normal presign requests instead of a
// dedicated background loop.
func (db *DB) PruneSegments(ctx context.Context, deviceID string, olderThanMs uint64, limit int) ([]SegmentRecord, error) {
	rows, err := db.pool.Query(ctx,
		`DELETE FROM segments
		 WHERE segment_id IN (
		   SELECT segment_id FROM segments
		   WHERE device_id = $1 AND start_ts < $2
		   ORDER BY start_ts
		   LIMIT $3
		 )
		 RETURNING segment_id, device_id, s3_key, start_ts, end_ts, size_bytes, resolution, created_at, has_motion`,
		deviceID, int64(olderThanMs), limit)
	if err != nil {
		return nil, fmt.Errorf("prune segments: %w", err)
	}
	defer rows.Close()

	var deleted []SegmentRecord
	for rows.Next() {
		var s SegmentRecord
		var startTS, endTS, sizeBytes, createdAt int64
		if err := rows.Scan(&s.SegmentID, &s.DeviceID, &s.S3Key, &startTS, &endTS, &sizeBytes, &s.Resolution, &createdAt, &s.HasMotion); err != nil {
			return nil, fmt.Errorf("scanning pruned segment: %w", err)
		}
		s.StartTS = uint64(startTS)
		s.EndTS = uint64(endTS)
		s.SizeBytes = uint64(sizeBytes)
		s.CreatedAt = uint64(createdAt)
		deleted = append(deleted, s)
	}
	return deleted, rows.Err()
}

// DeleteSegmentsRange deletes up to `limit` segments for deviceID whose
// start_ts falls in [fromMs, toMs]. When toMs is 0 the upper bound is
// ignored, so (fromMs=0, toMs=0) deletes every segment for the device.
// Returns the full deleted rows so the caller can reap the matching S3
// objects and decrement the storage counter. Bounded by LIMIT so
// handlers can loop and report progress to the UI.
//
// Matching is on start_ts only (mirrors ListSegments / ListSegmentCoverage),
// not on the segment's full [start_ts, end_ts] extent. A segment whose
// start_ts lies inside the range is deleted wholesale even if its
// end_ts runs past toMs; a segment that began before fromMs but
// overlaps into the range is not deleted. In the UI this means the
// first/last segments of a visually selected clip may be only
// partially covered — acceptable because segments are short (~6s).
func (db *DB) DeleteSegmentsRange(ctx context.Context, deviceID string, fromMs, toMs uint64, limit int) ([]SegmentRecord, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if toMs == 0 {
		rows, err = db.pool.Query(ctx,
			`DELETE FROM segments
			 WHERE segment_id IN (
			   SELECT segment_id FROM segments
			   WHERE device_id = $1 AND start_ts >= $2
			   ORDER BY start_ts
			   LIMIT $3
			 )
			 RETURNING segment_id, device_id, s3_key, start_ts, end_ts, size_bytes, resolution, created_at, has_motion`,
			deviceID, int64(fromMs), limit)
	} else {
		rows, err = db.pool.Query(ctx,
			`DELETE FROM segments
			 WHERE segment_id IN (
			   SELECT segment_id FROM segments
			   -- Range matched on start_ts, not extent: a segment starting just
			   -- before fromMs is kept even if it extends into the range, and one
			   -- starting inside the range is deleted even if end_ts exceeds toMs.
			   WHERE device_id = $1 AND start_ts >= $2 AND start_ts <= $3
			   ORDER BY start_ts
			   LIMIT $4
			 )
			 RETURNING segment_id, device_id, s3_key, start_ts, end_ts, size_bytes, resolution, created_at, has_motion`,
			deviceID, int64(fromMs), int64(toMs), limit)
	}
	if err != nil {
		return nil, fmt.Errorf("delete segments range: %w", err)
	}
	defer rows.Close()

	var deleted []SegmentRecord
	for rows.Next() {
		var s SegmentRecord
		var startTS, endTS, sizeBytes, createdAt int64
		if err := rows.Scan(&s.SegmentID, &s.DeviceID, &s.S3Key, &startTS, &endTS, &sizeBytes, &s.Resolution, &createdAt, &s.HasMotion); err != nil {
			return nil, fmt.Errorf("scanning deleted segment: %w", err)
		}
		s.StartTS = uint64(startTS)
		s.EndTS = uint64(endTS)
		s.SizeBytes = uint64(sizeBytes)
		s.CreatedAt = uint64(createdAt)
		deleted = append(deleted, s)
	}
	return deleted, rows.Err()
}

// CountSegmentsRange returns the number of segments for deviceID whose
// start_ts falls in [fromMs, toMs]. When toMs is 0 the upper bound is
// ignored. Uses the (device_id, start_ts) index so it's fast even for
// large tables.
func (db *DB) CountSegmentsRange(ctx context.Context, deviceID string, fromMs, toMs uint64) (int, error) {
	var count int
	var err error
	if toMs == 0 {
		err = db.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM segments WHERE device_id = $1 AND start_ts >= $2`,
			deviceID, int64(fromMs)).Scan(&count)
	} else {
		err = db.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM segments WHERE device_id = $1 AND start_ts >= $2 AND start_ts <= $3`,
			deviceID, int64(fromMs), int64(toMs)).Scan(&count)
	}
	if err != nil {
		return 0, fmt.Errorf("count segments range: %w", err)
	}
	return count, nil
}

func (db *DB) LatestSegment(ctx context.Context, deviceID string) (*SegmentRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT segment_id, device_id, s3_key, start_ts, end_ts, size_bytes, resolution, created_at, has_motion
		 FROM segments WHERE device_id = $1 ORDER BY start_ts DESC LIMIT 1`, deviceID)

	var s SegmentRecord
	var startTS, endTS, sizeBytes, createdAt int64
	err := row.Scan(&s.SegmentID, &s.DeviceID, &s.S3Key, &startTS, &endTS, &sizeBytes, &s.Resolution, &createdAt, &s.HasMotion)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest segment: %w", err)
	}
	s.StartTS = uint64(startTS)
	s.EndTS = uint64(endTS)
	s.SizeBytes = uint64(sizeBytes)
	s.CreatedAt = uint64(createdAt)
	return &s, nil
}
