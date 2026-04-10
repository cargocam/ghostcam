// Package db provides the PostgreSQL database layer.
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/cargocam/ghostcam/server/auth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps a pgxpool connection. It is the only database implementation —
// tests cover pure functions, not DB code, so there's no interface.
type DB struct {
	pool *pgxpool.Pool
}

// Connect creates a new DB and runs migrations.
func Connect(ctx context.Context, databaseURL string) (*DB, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing database URL: %w", err)
	}
	config.MaxConns = 20

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	db := &DB{pool: pool}
	if err := db.RunMigrations(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return db, nil
}

// Close closes the database pool.
func (db *DB) Close() {
	db.pool.Close()
}

// Initialize performs first-run setup: creates admin user if no users exist,
// ensures HMAC secret exists. Returns the initial password if one was generated.
func (db *DB) Initialize(ctx context.Context, presetPassword, adminEmail string) (string, error) {
	var hasUsers bool
	err := db.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users)").Scan(&hasUsers)
	if err != nil {
		return "", fmt.Errorf("checking users: %w", err)
	}

	var initialPassword string
	if !hasUsers {
		password := presetPassword
		if password == "" {
			password = auth.GenerateRandomPassword()
		}
		hash, err := auth.HashPassword(password)
		if err != nil {
			return "", fmt.Errorf("hashing password: %w", err)
		}

		userID := uuid.New().String()
		now := time.Now().Unix()
		_, err = db.pool.Exec(ctx,
			`INSERT INTO users (user_id, email, password_hash, display_name, created_at, password_changed_at)
			 VALUES ($1, $2, $3, 'Admin', $4, $4)`,
			userID, adminEmail, hash, now)
		if err != nil {
			return "", fmt.Errorf("creating admin user: %w", err)
		}
		// Auto-create subscription for admin on the free tier.
		// Paid tiers require an active Stripe subscription. In dev mode
		// (Stripe not configured), effectiveTier() returns "enterprise"
		// so tier limits are not enforced regardless of this value.
		if err := db.CreateSubscription(ctx, userID, "free", "active"); err != nil {
			slog.Warn("failed to create admin subscription", "error", err)
		}
		initialPassword = password
	}

	// Ensure HMAC secret exists
	var secretExists bool
	err = db.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM config WHERE key = 'hmac_secret')").Scan(&secretExists)
	if err != nil {
		return "", fmt.Errorf("checking HMAC secret: %w", err)
	}
	if !secretExists {
		secret := auth.GenerateHMACSecret()
		_, err = db.pool.Exec(ctx, "INSERT INTO config (key, value) VALUES ('hmac_secret', $1)", secret)
		if err != nil {
			return "", fmt.Errorf("storing HMAC secret: %w", err)
		}
		slog.Info("generated new HMAC secret")
	}

	return initialPassword, nil
}

// nowUnix returns the current time as Unix seconds.
func nowUnix() int64 {
	return time.Now().Unix()
}

// Record types

// CameraRecord is a camera from the database.
type CameraRecord struct {
	DeviceID      string
	UserID        *string
	DisplayName   string
	EnrolledAt    int64
	LastSeenAt    *int64
	Notes         *string
	Resolution    string
	RecordingMode string
}

// CameraUpdate holds optional fields for updating a camera.
type CameraUpdate struct {
	DisplayName   *string
	Notes         *string
	Resolution    *string
	RecordingMode *string
}

// UserRecord is a user from the database.
type UserRecord struct {
	UserID      string
	Email       string
	DisplayName string
	CreatedAt   int64
	VerifiedAt  *int64
	DisabledAt  *int64
}

// NewAPIToken holds fields for creating an API token.
type NewAPIToken struct {
	TokenID   string
	UserID    string
	TokenHash string
	Label     string
	ExpiresAt *int64
}

// APITokenRecord is an API token from the database.
type APITokenRecord struct {
	TokenID    string
	UserID     string
	Label      string
	CreatedAt  int64
	ExpiresAt  *int64
	LastUsedAt *int64
}

// SegmentRecord is a segment metadata record.
type SegmentRecord struct {
	SegmentID  string
	DeviceID   string
	S3Key      string
	StartTS    uint64
	EndTS      uint64
	SizeBytes  uint64
	Resolution string
	CreatedAt  uint64
	HasMotion  bool
}

// CoverageRecord is a lightweight segment record for timeline coverage.
type CoverageRecord struct {
	SegmentID string
	StartTS   uint64
	EndTS     uint64
	HasMotion bool
}

// SubscriptionRecord is a subscription from the database.
type SubscriptionRecord struct {
	UserID               string
	Tier                 string
	Status               string
	StripeCustomerID     *string
	StripeSubscriptionID *string
}

// SubscriptionUpdate holds optional fields for updating a subscription.
type SubscriptionUpdate struct {
	Tier                 *string
	Status               *string
	StripeCustomerID     *string
	StripeSubscriptionID *string
}

// AuditLogRecord is an audit log entry from the database.
type AuditLogRecord struct {
	ID        int64           `json:"id"`
	Timestamp string          `json:"timestamp"`
	EventType string          `json:"event_type"`
	EventData json.RawMessage `json:"event_data"`
	HMAC      string          `json:"hmac"`
}
