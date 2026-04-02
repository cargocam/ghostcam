package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (db *PostgresDB) GetSubscription(ctx context.Context, userID string) (*SubscriptionRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT user_id, tier, status FROM subscriptions WHERE user_id = $1`, userID)

	var s SubscriptionRecord
	err := row.Scan(&s.UserID, &s.Tier, &s.Status)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get subscription: %w", err)
	}
	return &s, nil
}

func (db *PostgresDB) CreateSubscription(ctx context.Context, userID, tier, status string) error {
	now := time.Now().Unix()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO subscriptions (user_id, tier, status, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)
		 ON CONFLICT (user_id) DO NOTHING`,
		userID, tier, status, now)
	if err != nil {
		return fmt.Errorf("create subscription: %w", err)
	}
	return nil
}

func (db *PostgresDB) GetCameraCount(ctx context.Context, userID string) (int64, error) {
	var count int64
	err := db.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM cameras WHERE user_id = $1", userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get camera count: %w", err)
	}
	return count, nil
}

func (db *PostgresDB) GetUserStorageBytes(ctx context.Context, userID string) (uint64, error) {
	var total int64
	err := db.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(s.size_bytes), 0)
		 FROM segments s JOIN cameras c ON s.device_id = c.device_id
		 WHERE c.user_id = $1`, userID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("get user storage: %w", err)
	}
	return uint64(total), nil
}
