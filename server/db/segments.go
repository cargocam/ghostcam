package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (db *DB) InsertSegments(ctx context.Context, segments []SegmentRecord) error {
	if len(segments) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, s := range segments {
		batch.Queue(
			`INSERT INTO segments (segment_id, device_id, s3_key, start_ts, end_ts, size_bytes, resolution, created_at, has_motion)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 ON CONFLICT (segment_id) DO NOTHING`,
			s.SegmentID, s.DeviceID, s.S3Key, int64(s.StartTS), int64(s.EndTS), int64(s.SizeBytes), s.Resolution, int64(s.CreatedAt), s.HasMotion)
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
		`SELECT segment_id, start_ts, end_ts, has_motion
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
		if err := rows.Scan(&r.SegmentID, &startTS, &endTS, &r.HasMotion); err != nil {
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
