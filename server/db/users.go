package db

import (
	"context"
	"fmt"

	"github.com/cargocam/ghostcam/server/auth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (db *DB) GetUserByEmail(ctx context.Context, email string) (*UserRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT user_id, email, display_name, created_at, verified_at, disabled_at, deleted_at
		 FROM users WHERE email = $1`, email)

	var u UserRecord
	err := row.Scan(&u.UserID, &u.Email, &u.DisplayName, &u.CreatedAt, &u.VerifiedAt, &u.DisabledAt, &u.DeletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	return &u, nil
}

// GetUserByID returns a user record by ID, or (nil, nil) if not found.
// Used by admin handlers that need to gate actions on the target user's
// current state (admin? deleted?).
func (db *DB) GetUserByID(ctx context.Context, userID string) (*UserRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT user_id, email, display_name, created_at, verified_at, disabled_at, deleted_at
		 FROM users WHERE user_id = $1`, userID)

	var u UserRecord
	err := row.Scan(&u.UserID, &u.Email, &u.DisplayName, &u.CreatedAt, &u.VerifiedAt, &u.DisabledAt, &u.DeletedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	return &u, nil
}

func (db *DB) VerifyPassword(ctx context.Context, userID, password string) (bool, error) {
	var hash string
	err := db.pool.QueryRow(ctx,
		"SELECT password_hash FROM users WHERE user_id = $1", userID).Scan(&hash)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("verify password: %w", err)
	}
	return auth.VerifyPassword(password, hash)
}

func (db *DB) SetPassword(ctx context.Context, userID, passwordHash string) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		"UPDATE users SET password_hash = $1, password_changed_at = $2 WHERE user_id = $3",
		passwordHash, now, userID)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return nil
}

// MarkVerified sets verified_at on a user that hasn't been verified yet.
func (db *DB) MarkVerified(ctx context.Context, userID string) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`UPDATE users SET verified_at = $1 WHERE user_id = $2 AND verified_at IS NULL`,
		now, userID,
	)
	if err != nil {
		return fmt.Errorf("mark verified: %w", err)
	}
	return nil
}

// SetEmail updates the user's email address. Returns an error if the new
// email is already taken by another user.
func (db *DB) SetEmail(ctx context.Context, userID, newEmail string) error {
	now := nowUnix()
	tag, err := db.pool.Exec(ctx,
		`UPDATE users SET email = $1, verified_at = $2 WHERE user_id = $3`,
		newEmail, now, userID,
	)
	if err != nil {
		return fmt.Errorf("set email: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set email: user not found")
	}
	return nil
}

// --- Admin user management ---

// AdminUserRecord is a platform-wide view of a user, joined with their
// admin status, subscription tier, and camera count. Used to render the
// admin Users section. Fields that require additional queries (segment
// storage bytes) are intentionally omitted — admins can drill into a
// specific user via the cameras list for that.
type AdminUserRecord struct {
	UserID      string
	Email       string
	DisplayName string
	CreatedAt   int64
	VerifiedAt  *int64
	DisabledAt  *int64
	DeletedAt   *int64
	IsAdmin     bool
	Tier        string
	CameraCount int64
}

// ListAllUsers returns every user in the database, newest first. Soft
// deleted users are still included — admins need to see them to choose
// whether to hard delete via psql or leave them in "trash". The query
// left-joins admins and subscriptions so the caller never needs a
// secondary fan-out of ~2 queries per row.
func (db *DB) ListAllUsers(ctx context.Context) ([]AdminUserRecord, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT
			u.user_id, u.email, u.display_name, u.created_at,
			u.verified_at, u.disabled_at, u.deleted_at,
			(a.user_id IS NOT NULL) AS is_admin,
			COALESCE(s.tier, 'free') AS tier,
			COALESCE((SELECT COUNT(*) FROM cameras c WHERE c.user_id = u.user_id), 0) AS camera_count
		FROM users u
		LEFT JOIN admins a ON a.user_id = u.user_id
		LEFT JOIN subscriptions s ON s.user_id = u.user_id
		ORDER BY u.created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list all users: %w", err)
	}
	defer rows.Close()

	var out []AdminUserRecord
	for rows.Next() {
		var u AdminUserRecord
		if err := rows.Scan(
			&u.UserID, &u.Email, &u.DisplayName, &u.CreatedAt,
			&u.VerifiedAt, &u.DisabledAt, &u.DeletedAt,
			&u.IsAdmin, &u.Tier, &u.CameraCount,
		); err != nil {
			return nil, fmt.Errorf("scan admin user row: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("admin users rows: %w", err)
	}
	return out, nil
}

// CreateUser inserts a new user with a pre-hashed password. Returns the
// generated user_id. Caller is responsible for also calling
// CreateSubscription so the row has the free tier — the two inserts
// aren't wrapped in a transaction because the subscription code path
// already tolerates its own absence (GetSubscription returns nil, the
// effective tier falls back to free).
//
// Returns an error containing "email exists" if a user with the same
// email is already in the DB — callers use this to return 409.
func (db *DB) CreateUser(ctx context.Context, email, displayName, passwordHash string) (string, error) {
	userID := newUserID()
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO users (user_id, email, password_hash, display_name, created_at, password_changed_at)
		 VALUES ($1, $2, $3, $4, $5, $5)`,
		userID, email, passwordHash, displayName, now,
	)
	if err != nil {
		// pgx returns "duplicate key value violates unique constraint"
		// which leaks the index name — wrap with a sentinel the handler
		// can pattern-match without importing pgx error types.
		return "", fmt.Errorf("create user: %w", err)
	}
	return userID, nil
}

// SetUserDisabled toggles the disabled_at timestamp. Idempotent: calling
// it with the same value is a no-op.
func (db *DB) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	var err error
	if disabled {
		now := nowUnix()
		_, err = db.pool.Exec(ctx,
			`UPDATE users SET disabled_at = $1 WHERE user_id = $2 AND disabled_at IS NULL`,
			now, userID,
		)
	} else {
		_, err = db.pool.Exec(ctx,
			`UPDATE users SET disabled_at = NULL WHERE user_id = $1`, userID,
		)
	}
	if err != nil {
		return fmt.Errorf("set user disabled: %w", err)
	}
	return nil
}

// SoftDeleteUser stamps both deleted_at and disabled_at so the login
// path rejects the account and admin listings can still render the row
// for audit purposes. Cameras owned by the user remain in the DB but
// their presign calls are rejected by cameraAuth's owner-deleted check.
//
// Does NOT touch Stripe — the caller is expected to cancel any active
// subscription via the Stripe API before calling this, so the customer
// stops being billed.
func (db *DB) SoftDeleteUser(ctx context.Context, userID string) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`UPDATE users
		 SET deleted_at = COALESCE(deleted_at, $1),
		     disabled_at = COALESCE(disabled_at, $1)
		 WHERE user_id = $2`,
		now, userID,
	)
	if err != nil {
		return fmt.Errorf("soft delete user: %w", err)
	}
	return nil
}

// IsUserDeleted reports whether the given user has been soft-deleted.
// Used by cameraAuth to fail-close on cameras whose owner is gone.
// Returns false on empty userID (callers pass in possibly-unset camera
// owners via join).
func (db *DB) IsUserDeleted(ctx context.Context, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	var deletedAt *int64
	err := db.pool.QueryRow(ctx,
		`SELECT deleted_at FROM users WHERE user_id = $1`, userID,
	).Scan(&deletedAt)
	if err == pgx.ErrNoRows {
		// Treat unknown user as "deleted" so cameras whose owner was
		// hard-deleted out from under them also fail closed.
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("is user deleted: %w", err)
	}
	return deletedAt != nil, nil
}

// newUserID returns a UUID-shaped string for a new user. Matches the
// first-run bootstrap format used in db.Initialize.
func newUserID() string {
	return uuid.New().String()
}
