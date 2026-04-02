package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (db *PostgresDB) CreateProvisionToken(ctx context.Context, tokenHash, userID string, expiresAt int64) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO provision_tokens (token_hash, user_id, created_at, expires_at)
		 VALUES ($1, $2, $3, $4)`,
		tokenHash, userID, now, expiresAt)
	if err != nil {
		return fmt.Errorf("create provision token: %w", err)
	}
	return nil
}

// ClaimProvisionToken atomically claims a provision token. Returns the user_id if valid and unclaimed.
func (db *PostgresDB) ClaimProvisionToken(ctx context.Context, tokenHash, _ string) (*string, error) {
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
