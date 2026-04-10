package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (db *DB) CreateAPIToken(ctx context.Context, token *NewAPIToken) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO api_tokens (token_id, user_id, token_hash, label, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		token.TokenID, token.UserID, token.TokenHash, token.Label, now, token.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create api token: %w", err)
	}
	return nil
}

func (db *DB) ListAPITokens(ctx context.Context, userID string) ([]APITokenRecord, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT token_id, user_id, label, created_at, expires_at, last_used_at
		 FROM api_tokens WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()

	var tokens []APITokenRecord
	for rows.Next() {
		var t APITokenRecord
		if err := rows.Scan(&t.TokenID, &t.UserID, &t.Label, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scanning api token: %w", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// VerifyAPIToken looks up a token by its HMAC hash and returns the record if
// it exists and has not expired. Expired tokens are deleted on the fly so the
// api_tokens table doesn't accumulate dead rows.
func (db *DB) VerifyAPIToken(ctx context.Context, tokenHash string) (*APITokenRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT token_id, user_id, label, created_at, expires_at, last_used_at
		 FROM api_tokens WHERE token_hash = $1`, tokenHash)

	var t APITokenRecord
	err := row.Scan(&t.TokenID, &t.UserID, &t.Label, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("verify api token: %w", err)
	}

	now := nowUnix()
	if t.ExpiresAt != nil && *t.ExpiresAt <= now {
		_, _ = db.pool.Exec(ctx, "DELETE FROM api_tokens WHERE token_id = $1", t.TokenID)
		return nil, nil
	}

	// Update last_used_at (best-effort).
	_, _ = db.pool.Exec(ctx,
		"UPDATE api_tokens SET last_used_at = $1 WHERE token_id = $2", now, t.TokenID)
	t.LastUsedAt = &now

	return &t, nil
}

func (db *DB) DeleteAPIToken(ctx context.Context, tokenID string) error {
	_, err := db.pool.Exec(ctx, "DELETE FROM api_tokens WHERE token_id = $1", tokenID)
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}
	return nil
}
