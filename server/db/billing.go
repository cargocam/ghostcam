package db

import (
	"context"
	"fmt"

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

func (db *PostgresDB) GetCameraCount(ctx context.Context, userID string) (int64, error) {
	var count int64
	err := db.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM cameras WHERE user_id = $1", userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get camera count: %w", err)
	}
	return count, nil
}
