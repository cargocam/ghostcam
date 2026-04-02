package db

import (
	"context"
	"fmt"

	"github.com/cargocam/ghostcam/server/auth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (db *PostgresDB) CreateUser(ctx context.Context, email, passwordHash, displayName string) (string, error) {
	userID := uuid.New().String()
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO users (user_id, email, password_hash, display_name, created_at, password_changed_at)
		 VALUES ($1, $2, $3, $4, $5, $5)`,
		userID, email, passwordHash, displayName, now)
	if err != nil {
		return "", fmt.Errorf("create user: %w", err)
	}
	return userID, nil
}

func (db *PostgresDB) GetUserByEmail(ctx context.Context, email string) (*UserRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT user_id, email, display_name, created_at, verified_at, disabled_at
		 FROM users WHERE email = $1`, email)

	var u UserRecord
	err := row.Scan(&u.UserID, &u.Email, &u.DisplayName, &u.CreatedAt, &u.VerifiedAt, &u.DisabledAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	return &u, nil
}

func (db *PostgresDB) VerifyPassword(ctx context.Context, userID, password string) (bool, error) {
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

func (db *PostgresDB) SetPassword(ctx context.Context, userID, passwordHash string) error {
	now := nowUnix()
	_, err := db.pool.Exec(ctx,
		"UPDATE users SET password_hash = $1, password_changed_at = $2 WHERE user_id = $3",
		passwordHash, now, userID)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return nil
}
