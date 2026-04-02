package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (db *PostgresDB) InsertSegments(ctx context.Context, segments []SegmentRecord) error {
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

func (db *PostgresDB) ListSegments(ctx context.Context, deviceID string, fromTS, toTS uint64) ([]SegmentRecord, error) {
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

func (db *PostgresDB) LatestSegment(ctx context.Context, deviceID string) (*SegmentRecord, error) {
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
