// Package handlers implements HTTP handlers for the Ghostcam API.
package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/cargocam/ghostcam/server/s3"
)

// defaultTierID is the tier assigned to users without a paid Stripe subscription.
const defaultTierID = "free"

// effectiveTier returns the user's billing tier. Paid tiers require an active
// Stripe subscription — a tier column alone is not enough. When Stripe is not
// configured (dev/local), returns "enterprise" (unlimited) so testing works
// without payment infrastructure.
func effectiveTier(sub *db.SubscriptionRecord, stripeConfigured bool) string {
	if !stripeConfigured {
		return "enterprise" // dev mode: unlimited
	}
	if sub == nil {
		return defaultTierID
	}
	// Paid tiers require an active Stripe subscription
	if sub.Tier != defaultTierID && (sub.StripeSubscriptionID == nil || sub.Status != "active") {
		return defaultTierID
	}
	return sub.Tier
}

// StripeConfig holds Stripe-specific configuration. All fields are empty when
// billing is disabled (STRIPE_SECRET_KEY not set).
type StripeConfig struct {
	SecretKey          string
	WebhookSecret      string
	PriceIDStarter     string
	PriceIDPro         string
	PriceIDEnterprise  string
	PortalConfigID     string
}

// Handlers holds all HTTP handler methods and their shared dependencies.
type Handlers struct {
	DB             db.Database
	Redis          *redis.Client
	S3             *s3.Client
	HMACSecret     []byte
	PresignTTLSecs uint64
	AdminEmail     string
	PublicURL      string // configured public URL for QR codes etc.
	SecureCookies  bool   // set Secure flag on auth cookies (true behind TLS)
	RetentionDays  int    // segment retention period (for coverage window default)
	Stripe         StripeConfig
}

// New creates a new Handlers instance.
func New(database db.Database, redisClient *redis.Client, s3Client *s3.Client, hmacSecret []byte, presignTTLSecs uint64, adminEmail, publicURL string, secureCookies bool, retentionDays int, stripe StripeConfig) *Handlers {
	return &Handlers{
		DB:             database,
		Redis:          redisClient,
		S3:             s3Client,
		HMACSecret:     hmacSecret,
		PresignTTLSecs: presignTTLSecs,
		AdminEmail:     adminEmail,
		PublicURL:      publicURL,
		SecureCookies:  secureCookies,
		RetentionDays:  retentionDays,
		Stripe:         stripe,
	}
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
