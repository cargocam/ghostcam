package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CreateProvisionToken inserts a new token. Expired, unclaimed tokens for the
// same user are deleted in the same transaction so stale rows don't accumulate
// (avoids a dedicated cleanup loop).
func (db *DB) CreateProvisionToken(ctx context.Context, tokenHash, userID string, expiresAt int64) error {
	now := nowUnix()
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("create provision token: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM provision_tokens
		 WHERE user_id = $1 AND claimed_at IS NULL AND expires_at < $2`,
		userID, now); err != nil {
		return fmt.Errorf("prune expired provision tokens: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO provision_tokens (token_hash, user_id, created_at, expires_at)
		 VALUES ($1, $2, $3, $4)`,
		tokenHash, userID, now, expiresAt); err != nil {
		return fmt.Errorf("create provision token: %w", err)
	}

	return tx.Commit(ctx)
}

// ClaimProvisionToken atomically claims a provision token. Returns the user_id if valid and unclaimed.
func (db *DB) ClaimProvisionToken(ctx context.Context, tokenHash string) (*string, error) {
	now := nowUnix()
	row := db.pool.QueryRow(ctx,
		`UPDATE provision_tokens
		 SET claimed_at = $1
		 WHERE token_hash = $2 AND claimed_at IS NULL AND expires_at > $1
		 RETURNING user_id`,
		now, tokenHash)

	var userID string
	err := row.Scan(&userID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim provision token: %w", err)
	}
	return &userID, nil
}
