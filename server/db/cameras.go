package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (db *DB) GetCamera(ctx context.Context, deviceID string) (*CameraRecord, error) {
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

func (db *DB) ListCameras(ctx context.Context, userID string) ([]CameraRecord, error) {
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

func (db *DB) UpdateCamera(ctx context.Context, deviceID string, update *CameraUpdate) error {
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

// TouchCameraLastSeen updates last_seen_at to the current unix timestamp (seconds).
func (db *DB) TouchCameraLastSeen(ctx context.Context, deviceID string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE cameras SET last_seen_at = $1 WHERE device_id = $2`,
		nowUnix(), deviceID)
	if err != nil {
		return fmt.Errorf("touch camera last_seen_at: %w", err)
	}
	return nil
}

func (db *DB) DeleteCamera(ctx context.Context, deviceID string) error {
	_, err := db.pool.Exec(ctx, "DELETE FROM cameras WHERE device_id = $1", deviceID)
	if err != nil {
		return fmt.Errorf("delete camera: %w", err)
	}
	return nil
}

func (db *DB) CreateProvisionedCamera(ctx context.Context, deviceID, userID, deviceSerial string) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO cameras (device_id, user_id, display_name, enrolled_at, device_serial)
		 VALUES ($1, $2, 'New Camera', $3, $4)`,
		deviceID, userID, now, deviceSerial)
	if err != nil {
		return fmt.Errorf("create provisioned camera: %w", err)
	}
	return nil
}

func (db *DB) GetCameraBySerial(ctx context.Context, deviceSerial string) (*CameraRecord, error) {
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

// --- Camera public keys (ed25519 signature auth) ---

// UpsertCameraPublicKey stores or replaces the ed25519 public key for a
// camera. Used during v2 provisioning and legacy→v2 key registration.
func (db *DB) UpsertCameraPublicKey(ctx context.Context, deviceID, publicKeyHex string) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO camera_public_keys (device_id, public_key, created_at, updated_at)
		 VALUES ($1, $2, $3, $3)
		 ON CONFLICT (device_id) DO UPDATE SET public_key = $2, updated_at = $3`,
		deviceID, publicKeyHex, now)
	if err != nil {
		return fmt.Errorf("upsert camera public key: %w", err)
	}
	return nil
}

// GetCameraPublicKey returns the hex-encoded ed25519 public key for a
// device. Returns empty string (not error) if no key is registered.
func (db *DB) GetCameraPublicKey(ctx context.Context, deviceID string) (string, error) {
	var pubKey string
	err := db.pool.QueryRow(ctx,
		`SELECT public_key FROM camera_public_keys WHERE device_id = $1`,
		deviceID).Scan(&pubKey)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get camera public key: %w", err)
	}
	return pubKey, nil
}

// --- Admin camera management ---

// AdminCameraRecord is a platform-wide view of a camera joined with its
// owning user's email. Used to render the admin Cameras section. Omits
// notes/resolution/recording_mode to keep the payload small — admins
// who need those details can drop down to the user's settings page.
type AdminCameraRecord struct {
	DeviceID    string
	DisplayName string
	UserID      string
	OwnerEmail  string
	EnrolledAt  int64
	LastSeenAt  *int64
}

// ListAllCameras returns every camera in the database joined with its
// owner's email, newest-enrolled first. Includes cameras whose owner is
// soft-deleted so admins can see the full fleet and act on orphans.
func (db *DB) ListAllCameras(ctx context.Context) ([]AdminCameraRecord, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT c.device_id, c.display_name, c.user_id, u.email,
		       c.enrolled_at, c.last_seen_at
		FROM cameras c
		JOIN users u ON u.user_id = c.user_id
		ORDER BY c.enrolled_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list all cameras: %w", err)
	}
	defer rows.Close()

	var out []AdminCameraRecord
	for rows.Next() {
		var c AdminCameraRecord
		if err := rows.Scan(
			&c.DeviceID, &c.DisplayName, &c.UserID, &c.OwnerEmail,
			&c.EnrolledAt, &c.LastSeenAt,
		); err != nil {
			return nil, fmt.Errorf("scan admin camera row: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("admin cameras rows: %w", err)
	}
	return out, nil
}

// ReassignCamera changes the owning user of a camera. Caller is
// responsible for enforcing tier-limit invariants before calling — this
// is a raw UPDATE that doesn't know about billing.
func (db *DB) ReassignCamera(ctx context.Context, deviceID, newUserID string) error {
	tag, err := db.pool.Exec(ctx,
		`UPDATE cameras SET user_id = $1 WHERE device_id = $2`,
		newUserID, deviceID,
	)
	if err != nil {
		return fmt.Errorf("reassign camera: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("reassign camera: device %q not found", deviceID)
	}
	return nil
}
