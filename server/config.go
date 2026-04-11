package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/BurntSushi/toml"
)

// ServerConfig holds the fully resolved server configuration.
type ServerConfig struct {
	DataDir     string
	HTTPPort    uint16
	DatabaseURL string
	RedisURL    string // empty = disabled
	AdminEmail  string
	// Admin password preset (env only, sensitive)
	AdminPassword string
	// S3 / Tigris
	S3Bucket         string
	S3Region         string
	S3Endpoint       string // empty = AWS default
	S3PresignTTLSecs uint64
	// Public URL for QR codes (e.g. "https://cam.example.com")
	PublicURL string
	// Stripe (optional — billing disabled if StripeSecretKey is empty).
	// Tier/product IDs are NOT configured here; the server fetches active
	// prices from Stripe on startup and treats each product with the
	// `ghostcam_camera_limit` / `ghostcam_storage_gb` metadata keys as a
	// tier. See server/billing/tiers.go.
	StripeSecretKey      string
	StripeWebhookSecret  string
	StripePortalConfigID string
	// Segment retention in days (default 30)
	SegmentRetentionDays int
}

// secureCookies returns true when the PublicURL is served over HTTPS.
func (c *ServerConfig) secureCookies() bool {
	return len(c.PublicURL) >= 8 && c.PublicURL[:8] == "https://"
}

// retentionDays returns the effective segment retention period.
func (c *ServerConfig) retentionDays() int {
	if c.SegmentRetentionDays <= 0 {
		return 30
	}
	return c.SegmentRetentionDays
}

// serverConfigFile is the TOML-deserialized config file. All fields optional.
type serverConfigFile struct {
	DataDir    *string `toml:"data_dir"`
	HTTPPort   *uint16 `toml:"http_port"`
	RedisURL   *string `toml:"redis_url"`
	AdminEmail *string `toml:"admin_email"`
	S3Bucket   *string `toml:"s3_bucket"`
	S3Region   *string `toml:"s3_region"`
	S3Endpoint *string `toml:"s3_endpoint"`
}

// LoadConfig loads configuration with layering: defaults -> config file -> env vars.
func LoadConfig() (*ServerConfig, error) {
	file := loadConfigFile()

	cfg := &ServerConfig{
		DataDir:          envOrFileOrDefault("GHOSTCAM_DATA_DIR", file.DataDir, "/var/ghostcam"),
		HTTPPort:         envOrFileOrDefaultUint16("GHOSTCAM_HTTP_PORT", file.HTTPPort, 3000),
		AdminEmail:       envOrFileOrDefault("GHOSTCAM_ADMIN_EMAIL", file.AdminEmail, "admin@localhost"),
		S3Bucket:         envOrFileOrDefault("GHOSTCAM_S3_BUCKET", file.S3Bucket, "ghostcam-segments"),
		S3Region:         envOrFileOrDefault("GHOSTCAM_S3_REGION", file.S3Region, "auto"),
		S3Endpoint:       envOrFileOrDefault("GHOSTCAM_S3_ENDPOINT", file.S3Endpoint, ""),
		S3PresignTTLSecs: envOrDefaultUint64("GHOSTCAM_S3_PRESIGN_TTL_SECS", 3600),
	}

	// Sensitive: env only
	cfg.DatabaseURL = os.Getenv("GHOSTCAM_DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("GHOSTCAM_DATABASE_URL is required")
	}

	cfg.RedisURL = envOrFileOrDefault("GHOSTCAM_REDIS_URL", file.RedisURL, "")
	cfg.AdminPassword = os.Getenv("GHOSTCAM_ADMIN_PASSWORD")
	cfg.PublicURL = os.Getenv("GHOSTCAM_PUBLIC_URL")

	// Stripe (env only — sensitive)
	cfg.StripeSecretKey = os.Getenv("STRIPE_SECRET_KEY")
	cfg.StripeWebhookSecret = os.Getenv("STRIPE_WEBHOOK_SECRET")
	cfg.StripePortalConfigID = os.Getenv("STRIPE_PORTAL_CONFIG_ID")

	// Segment retention
	cfg.SegmentRetentionDays = int(envOrDefaultUint64("GHOSTCAM_SEGMENT_RETENTION_DAYS", 30))

	return cfg, nil
}

func loadConfigFile() serverConfigFile {
	var file serverConfigFile
	candidates := []string{}

	if v := os.Getenv("GHOSTCAM_CONFIG_FILE"); v != "" {
		candidates = append(candidates, v)
	}
	if v := os.Getenv("GHOSTCAM_DATA_DIR"); v != "" {
		candidates = append(candidates, v+"/server.toml")
	}
	candidates = append(candidates, "/etc/ghostcam/server.toml")

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			slog.Info("loading config file", "path", path)
			if _, err := toml.DecodeFile(path, &file); err != nil {
				slog.Warn("failed to parse config file", "path", path, "error", err)
			}
			break
		}
	}
	return file
}

func envOrFileOrDefault(key string, fileVal *string, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

func envOrFileOrDefaultUint16(key string, fileVal *uint16, def uint16) uint16 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 16); err == nil {
			return uint16(n)
		}
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

func envOrDefaultUint64(key string, def uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
