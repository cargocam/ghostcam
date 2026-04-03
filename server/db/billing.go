package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (db *PostgresDB) GetSubscription(ctx context.Context, userID string) (*SubscriptionRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT user_id, tier, status, stripe_customer_id, stripe_subscription_id FROM subscriptions WHERE user_id = $1`, userID)

	var s SubscriptionRecord
	err := row.Scan(&s.UserID, &s.Tier, &s.Status, &s.StripeCustomerID, &s.StripeSubscriptionID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get subscription: %w", err)
	}
	return &s, nil
}

func (db *PostgresDB) GetSubscriptionByStripeCustomer(ctx context.Context, stripeCustomerID string) (*SubscriptionRecord, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT user_id, tier, status, stripe_customer_id, stripe_subscription_id FROM subscriptions WHERE stripe_customer_id = $1`, stripeCustomerID)

	var s SubscriptionRecord
	err := row.Scan(&s.UserID, &s.Tier, &s.Status, &s.StripeCustomerID, &s.StripeSubscriptionID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get subscription by stripe customer: %w", err)
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

func (db *PostgresDB) UpdateSubscription(ctx context.Context, userID string, update *SubscriptionUpdate) error {
	now := time.Now().Unix()
	_, err := db.pool.Exec(ctx,
		`UPDATE subscriptions SET
		 tier = COALESCE($1, tier),
		 status = COALESCE($2, status),
		 stripe_customer_id = COALESCE($3, stripe_customer_id),
		 stripe_subscription_id = COALESCE($4, stripe_subscription_id),
		 updated_at = $5
		 WHERE user_id = $6`,
		update.Tier, update.Status, update.StripeCustomerID, update.StripeSubscriptionID, now, userID)
	if err != nil {
		return fmt.Errorf("update subscription: %w", err)
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

func (db *PostgresDB) CheckStripeEvent(ctx context.Context, eventID string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM stripe_events WHERE event_id = $1)", eventID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check stripe event: %w", err)
	}
	return exists, nil
}

func (db *PostgresDB) RecordStripeEvent(ctx context.Context, eventID string) error {
	now := time.Now().Unix()
	_, err := db.pool.Exec(ctx,
		"INSERT INTO stripe_events (event_id, processed_at) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		eventID, now)
	if err != nil {
		return fmt.Errorf("record stripe event: %w", err)
	}
	return nil
}
