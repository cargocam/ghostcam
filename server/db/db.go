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

// Database is the interface for all database operations.
type Database interface {
	// Camera operations
	GetCamera(ctx context.Context, deviceID string) (*CameraRecord, error)
	ListCameras(ctx context.Context, userID string) ([]CameraRecord, error)
	UpdateCamera(ctx context.Context, deviceID string, update *CameraUpdate) error
	TouchCameraLastSeen(ctx context.Context, deviceID string) error
	DeleteCamera(ctx context.Context, deviceID string) error
	CreateProvisionedCamera(ctx context.Context, deviceID, userID, deviceSerial string) error

	// Camera API keys
	GetCameraByAPIKey(ctx context.Context, apiKeyHash string) (*CameraRecord, error)
	GetCameraBySerial(ctx context.Context, deviceSerial string) (*CameraRecord, error)
	CreateCameraAPIKey(ctx context.Context, deviceID, apiKeyHash string) error
	DeleteCameraAPIKey(ctx context.Context, deviceID string) error

	// Provision tokens
	CreateProvisionToken(ctx context.Context, tokenHash, userID string, expiresAt int64) error
	ClaimProvisionToken(ctx context.Context, tokenHash, deviceID string) (*string, error)

	// Sessions
	CreateSession(ctx context.Context, session *NewSession) error
	GetSession(ctx context.Context, sessionID string) (*SessionRecord, error)
	DeleteSession(ctx context.Context, sessionID string) error
	ExtendSession(ctx context.Context, sessionID string) error
	CleanupExpiredSessions(ctx context.Context) (int64, error)

	// Users
	CreateUser(ctx context.Context, email, passwordHash, displayName string) (string, error)
	GetUserByEmail(ctx context.Context, email string) (*UserRecord, error)
	VerifyPassword(ctx context.Context, userID, password string) (bool, error)
	SetPassword(ctx context.Context, userID, passwordHash string) error

	// API tokens
	CreateAPIToken(ctx context.Context, token *NewAPIToken) error
	ListAPITokens(ctx context.Context, userID string) ([]APITokenRecord, error)
	VerifyAPIToken(ctx context.Context, tokenHash string) (*APITokenRecord, error)
	DeleteAPIToken(ctx context.Context, tokenID string) error

	// Segments
	InsertSegments(ctx context.Context, segments []SegmentRecord) error
	ListSegments(ctx context.Context, deviceID string, fromTS, toTS uint64) ([]SegmentRecord, error)
	ListSegmentCoverage(ctx context.Context, deviceID string, fromTS, toTS uint64) ([]CoverageRecord, error)
	LatestSegment(ctx context.Context, deviceID string) (*SegmentRecord, error)

	// Commands
	EnqueueCommand(ctx context.Context, deviceID string, command json.RawMessage) error
	ClaimCommands(ctx context.Context, deviceID string) ([]json.RawMessage, error)

	// Billing
	GetSubscription(ctx context.Context, userID string) (*SubscriptionRecord, error)
	GetSubscriptionByStripeCustomer(ctx context.Context, stripeCustomerID string) (*SubscriptionRecord, error)
	CreateSubscription(ctx context.Context, userID, tier, status string) error
	UpdateSubscription(ctx context.Context, userID string, update *SubscriptionUpdate) error
	GetCameraCount(ctx context.Context, userID string) (int64, error)
	GetUserStorageBytes(ctx context.Context, userID string) (uint64, error)

	// Cleanup
	DeleteOldSegments(ctx context.Context, olderThanMs uint64, batchSize int) ([]SegmentRecord, error)
	DeleteStaleUnclaimedCameras(ctx context.Context, olderThanUnix int64) (int64, error)
	DeleteExpiredProvisionTokens(ctx context.Context) (int64, error)

	// Stripe idempotency
	CheckStripeEvent(ctx context.Context, eventID string) (bool, error)
	RecordStripeEvent(ctx context.Context, eventID string) error

	// Audit
	InsertAuditEntry(ctx context.Context, timestamp, eventType string, eventData json.RawMessage, hmac string) error
	QueryAuditLog(ctx context.Context, eventType, since, until string, limit, offset int64) ([]AuditLogRecord, int64, error)

	// Server config
	GetHMACSecret(ctx context.Context) ([]byte, error)

	// Health
	HealthCheck(ctx context.Context) error
}

// PostgresDB implements Database using pgxpool.
type PostgresDB struct {
	pool *pgxpool.Pool
}

// Connect creates a new PostgresDB and runs migrations.
func Connect(ctx context.Context, databaseURL string) (*PostgresDB, error) {
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

	db := &PostgresDB{pool: pool}
	if err := db.RunMigrations(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return db, nil
}

// Close closes the database pool.
func (db *PostgresDB) Close() {
	db.pool.Close()
}

// Initialize performs first-run setup: creates admin user if no users exist,
// ensures HMAC secret exists. Returns the initial password if one was generated.
func (db *PostgresDB) Initialize(ctx context.Context, presetPassword, adminEmail string) (string, error) {
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

// nowMs returns the current time as Unix milliseconds.
func nowMs() uint64 {
	return uint64(time.Now().UnixMilli())
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

// NewSession holds fields for creating a session.
type NewSession struct {
	SessionID string
	UserID    string
	UserAgent string
	IPAddress string
}

// SessionRecord is a session from the database.
type SessionRecord struct {
	SessionID    string
	UserID       string
	CreatedAt    int64
	ExpiresAt    int64
	LastActiveAt *int64
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
	UserID             string
	Tier               string
	Status             string
	StripeCustomerID   *string
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
