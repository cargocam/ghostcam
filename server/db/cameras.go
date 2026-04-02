package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (db *PostgresDB) GetCamera(ctx context.Context, deviceID string) (*CameraRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT device_id, user_id, display_name, enrolled_at, last_seen_at, notes, resolution, recording_mode
		 FROM cameras WHERE device_id = $1`, deviceID)

	var c CameraRecord
	err := row.Scan(&c.DeviceID, &c.UserID, &c.DisplayName, &c.EnrolledAt, &c.LastSeenAt, &c.Notes, &c.Resolution, &c.RecordingMode)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get camera: %w", err)
	}
	return &c, nil
}

func (db *PostgresDB) ListCameras(ctx context.Context, userID string) ([]CameraRecord, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT device_id, user_id, display_name, enrolled_at, last_seen_at, notes, resolution, recording_mode
		 FROM cameras WHERE user_id = $1 ORDER BY enrolled_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("list cameras: %w", err)
	}
	defer rows.Close()

	var cameras []CameraRecord
	for rows.Next() {
		var c CameraRecord
		if err := rows.Scan(&c.DeviceID, &c.UserID, &c.DisplayName, &c.EnrolledAt, &c.LastSeenAt, &c.Notes, &c.Resolution, &c.RecordingMode); err != nil {
			return nil, fmt.Errorf("scanning camera: %w", err)
		}
		cameras = append(cameras, c)
	}
	return cameras, rows.Err()
}

func (db *PostgresDB) UpdateCamera(ctx context.Context, deviceID string, update *CameraUpdate) error {
	if update.DisplayName == nil && update.Notes == nil && update.Resolution == nil && update.RecordingMode == nil {
		return nil
	}
	_, err := db.pool.Exec(ctx,
		`UPDATE cameras SET
		 display_name = COALESCE($1, display_name),
		 notes = COALESCE($2, notes),
		 resolution = COALESCE($3, resolution),
		 recording_mode = COALESCE($4, recording_mode)
		 WHERE device_id = $5`,
		update.DisplayName, update.Notes, update.Resolution, update.RecordingMode, deviceID)
	if err != nil {
		return fmt.Errorf("update camera: %w", err)
	}
	return nil
}

func (db *PostgresDB) DeleteCamera(ctx context.Context, deviceID string) error {
	_, err := db.pool.Exec(ctx, "DELETE FROM cameras WHERE device_id = $1", deviceID)
	if err != nil {
		return fmt.Errorf("delete camera: %w", err)
	}
	return nil
}

func (db *PostgresDB) CreateProvisionedCamera(ctx context.Context, deviceID, userID, deviceSerial string) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO cameras (device_id, user_id, cert_fingerprint, display_name, enrolled_at, device_serial)
		 VALUES ($1, $2, $1, 'New Camera', $3, $4)`,
		deviceID, userID, now, deviceSerial)
	if err != nil {
		return fmt.Errorf("create provisioned camera: %w", err)
	}
	return nil
}

func (db *PostgresDB) GetCameraByAPIKey(ctx context.Context, apiKeyHash string) (*CameraRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT c.device_id, c.user_id, c.display_name, c.enrolled_at, c.last_seen_at, c.notes, c.resolution, c.recording_mode
		 FROM camera_api_keys k JOIN cameras c ON k.device_id = c.device_id
		 WHERE k.api_key_hash = $1`, apiKeyHash)

	var c CameraRecord
	err := row.Scan(&c.DeviceID, &c.UserID, &c.DisplayName, &c.EnrolledAt, &c.LastSeenAt, &c.Notes, &c.Resolution, &c.RecordingMode)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get camera by api key: %w", err)
	}
	return &c, nil
}

func (db *PostgresDB) GetCameraBySerial(ctx context.Context, deviceSerial string) (*CameraRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT device_id, user_id, display_name, enrolled_at, last_seen_at, notes, resolution, recording_mode
		 FROM cameras WHERE device_serial = $1`, deviceSerial)

	var c CameraRecord
	err := row.Scan(&c.DeviceID, &c.UserID, &c.DisplayName, &c.EnrolledAt, &c.LastSeenAt, &c.Notes, &c.Resolution, &c.RecordingMode)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get camera by serial: %w", err)
	}
	return &c, nil
}

func (db *PostgresDB) DeleteCameraAPIKey(ctx context.Context, deviceID string) error {
	_, err := db.pool.Exec(ctx, "DELETE FROM camera_api_keys WHERE device_id = $1", deviceID)
	if err != nil {
		return fmt.Errorf("delete camera api key: %w", err)
	}
	return nil
}

func (db *PostgresDB) CreateCameraAPIKey(ctx context.Context, deviceID, apiKeyHash string) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO camera_api_keys (device_id, api_key_hash, created_at) VALUES ($1, $2, $3)`,
		deviceID, apiKeyHash, now)
	if err != nil {
		return fmt.Errorf("create camera api key: %w", err)
	}
	return nil
}
