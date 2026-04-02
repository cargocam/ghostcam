// Package handlers implements HTTP handlers for the Ghostcam API.
package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/cargocam/ghostcam/server/s3"
)

// defaultTierID is the tier assigned to users without a subscription record.
// Billing is always on; users without a subscription are on the free tier.
const defaultTierID = "free"

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
}

// New creates a new Handlers instance.
func New(database db.Database, redisClient *redis.Client, s3Client *s3.Client, hmacSecret []byte, presignTTLSecs uint64, adminEmail, publicURL string, secureCookies bool) *Handlers {
	return &Handlers{
		DB:             database,
		Redis:          redisClient,
		S3:             s3Client,
		HMACSecret:     hmacSecret,
		PresignTTLSecs: presignTTLSecs,
		AdminEmail:     adminEmail,
		PublicURL:      publicURL,
		SecureCookies:  secureCookies,
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
