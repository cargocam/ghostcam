package db

import (
	"context"
	"fmt"
)

// IsAdmin reports whether the given user has a row in the admins table.
// Returns false on empty userID (treating it the same as "not an admin")
// so callers can pass unauthenticated IDs without a special case.
func (db *DB) IsAdmin(ctx context.Context, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM admins WHERE user_id = $1)`,
		userID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("is admin: %w", err)
	}
	return exists, nil
}

// GrantAdmin makes userID an admin. Idempotent: re-granting is a no-op.
func (db *DB) GrantAdmin(ctx context.Context, userID string) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO admins (user_id, created_at) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO NOTHING`,
		userID, nowUnix(),
	)
	if err != nil {
		return fmt.Errorf("grant admin: %w", err)
	}
	return nil
}

// RevokeAdmin removes admin status from userID. Idempotent.
func (db *DB) RevokeAdmin(ctx context.Context, userID string) error {
	_, err := db.pool.Exec(ctx, `DELETE FROM admins WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("revoke admin: %w", err)
	}
	return nil
}
