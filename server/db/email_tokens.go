package db

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math/big"
	"time"

	"github.com/cargocam/ghostcam/server/auth"
	"github.com/jackc/pgx/v5"
)

// EmailTokenRecord is a consumed email token row.
type EmailTokenRecord struct {
	UserID  string
	Purpose string
	Payload *string
}

// CreateEmailToken generates a random token, hashes it with HMAC, stores
// the hash in email_tokens, and returns the raw token for embedding in a
// URL or message. For OTP codes, call CreateEmailOTP instead.
func (db *DB) CreateEmailToken(ctx context.Context, hmacSecret []byte, userID, purpose string, payload *string, ttl time.Duration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	rawStr := base64.RawURLEncoding.EncodeToString(raw)

	return rawStr, db.insertEmailToken(ctx, hmacSecret, rawStr, userID, purpose, payload, ttl)
}

// CreateEmailOTP generates a 6-digit numeric code, hashes it with HMAC,
// stores the hash in email_tokens, and returns the plaintext code.
func (db *DB) CreateEmailOTP(ctx context.Context, hmacSecret []byte, userID, purpose string, ttl time.Duration) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("generating OTP: %w", err)
	}
	code := fmt.Sprintf("%06d", n.Int64())

	return code, db.insertEmailToken(ctx, hmacSecret, code, userID, purpose, nil, ttl)
}

func (db *DB) insertEmailToken(ctx context.Context, hmacSecret []byte, raw, userID, purpose string, payload *string, ttl time.Duration) error {
	hash := auth.HMACToken(raw, hmacSecret)
	now := nowUnix()
	expiresAt := time.Now().Add(ttl).Unix()

	_, err := db.pool.Exec(ctx,
		`INSERT INTO email_tokens (token_hash, user_id, purpose, payload, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		hash, userID, purpose, payload, now, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("insert email token: %w", err)
	}
	return nil
}

// ConsumeEmailToken looks up an unused, unexpired token by its raw value
// and purpose, atomically marks it used, and returns the row. Returns
// (nil, nil) if no matching token exists or it is expired/already used.
func (db *DB) ConsumeEmailToken(ctx context.Context, hmacSecret []byte, rawToken, purpose string) (*EmailTokenRecord, error) {
	hash := auth.HMACToken(rawToken, hmacSecret)
	now := nowUnix()

	var rec EmailTokenRecord
	err := db.pool.QueryRow(ctx,
		`UPDATE email_tokens
		 SET used_at = $1
		 WHERE token_hash = $2
		   AND purpose = $3
		   AND used_at IS NULL
		   AND expires_at > $1
		 RETURNING user_id, purpose, payload`,
		now, hash, purpose,
	).Scan(&rec.UserID, &rec.Purpose, &rec.Payload)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume email token: %w", err)
	}
	return &rec, nil
}

// ConsumeEmailOTP looks up an unused, unexpired OTP code, checks the
// attempt count (max 5), increments it on mismatch, and marks used on
// match. Returns the record on success, nil on not-found/expired/exhausted.
func (db *DB) ConsumeEmailOTP(ctx context.Context, hmacSecret []byte, userID, code string) (*EmailTokenRecord, error) {
	hash := auth.HMACToken(code, hmacSecret)
	now := nowUnix()

	// Try to consume directly — if the hash matches, this is a single
	// atomic step. The WHERE clause also enforces attempts < 5.
	var rec EmailTokenRecord
	err := db.pool.QueryRow(ctx,
		`UPDATE email_tokens
		 SET used_at = $1
		 WHERE token_hash = $2
		   AND user_id = $3
		   AND purpose = 'login_otp'
		   AND used_at IS NULL
		   AND expires_at > $1
		   AND attempts < 5
		 RETURNING user_id, purpose, payload`,
		now, hash, userID,
	).Scan(&rec.UserID, &rec.Purpose, &rec.Payload)
	if err == pgx.ErrNoRows {
		// Increment attempt counter on any live OTP for this user so
		// brute-force guessing is bounded. We don't know which row
		// they were trying to hit, so increment all live ones.
		db.pool.Exec(ctx,
			`UPDATE email_tokens
			 SET attempts = attempts + 1
			 WHERE user_id = $1
			   AND purpose = 'login_otp'
			   AND used_at IS NULL
			   AND expires_at > $2`,
			userID, now,
		)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume email OTP: %w", err)
	}
	return &rec, nil
}

// InvalidateEmailTokens marks all unused tokens for a user+purpose as
// used. Called before issuing a new token so stale links/codes stop
// working (prevents "two codes in inbox" confusion).
func (db *DB) InvalidateEmailTokens(ctx context.Context, userID, purpose string) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`UPDATE email_tokens
		 SET used_at = $1
		 WHERE user_id = $2
		   AND purpose = $3
		   AND used_at IS NULL`,
		now, userID, purpose,
	)
	if err != nil {
		return fmt.Errorf("invalidate email tokens: %w", err)
	}
	return nil
}
