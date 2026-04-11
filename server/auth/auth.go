// Package auth provides password hashing, HMAC token signing, random
// credential generation, and JWT sign/verify for stateless session cookies.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	argon2Time    = 1
	argon2Memory  = 64 * 1024
	argon2Threads = 4
	argon2KeyLen  = 32
	saltLen       = 16
)

// HashPassword hashes a password with Argon2id. Returns a PHC-formatted string.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}

	hash := argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword verifies a password against a PHC-formatted Argon2id hash.
func VerifyPassword(password, encoded string) (bool, error) {
	var version int
	var memory, time uint32
	var threads uint8
	var saltB64, hashB64 string

	_, err := fmt.Sscanf(encoded, "$argon2id$v=%d$m=%d,t=%d,p=%d$%s",
		&version, &memory, &time, &threads, &saltB64)
	if err != nil {
		return false, fmt.Errorf("parsing hash: %w", err)
	}

	// Split saltB64 into salt and hash parts (separated by $)
	parts := splitDollar(encoded)
	if len(parts) != 6 {
		return false, fmt.Errorf("invalid hash format: expected 6 parts, got %d", len(parts))
	}
	saltB64 = parts[4]
	hashB64 = parts[5]

	salt, err := base64.RawStdEncoding.DecodeString(saltB64)
	if err != nil {
		return false, fmt.Errorf("decoding salt: %w", err)
	}

	expectedHash, err := base64.RawStdEncoding.DecodeString(hashB64)
	if err != nil {
		return false, fmt.Errorf("decoding hash: %w", err)
	}

	computedHash := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(expectedHash)))

	return subtle.ConstantTimeCompare(computedHash, expectedHash) == 1, nil
}

// DummyVerify performs a password hash computation with the same parameters as
// a real verification. Used to equalize login timing for non-existent users,
// preventing user enumeration via response latency.
func DummyVerify(password string) {
	salt := make([]byte, saltLen)
	argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
}

func splitDollar(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '$' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// HMACToken computes HMAC-SHA256 of a raw token and returns the hex-encoded hash.
func HMACToken(rawToken string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(rawToken))
	return hex.EncodeToString(mac.Sum(nil))
}

// GenerateRandomPassword returns a 16-character alphanumeric random password.
func GenerateRandomPassword() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, 16)
	for i := range result {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		result[i] = charset[n.Int64()]
	}
	return string(result)
}

// GenerateHMACSecret returns a 32-byte random secret.
func GenerateHMACSecret() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return b
}

// --- JWT ---

// JWTClaims holds the decoded claims from a verified JWT.
//
// IsAdmin is a UI hint only — stamped from db.IsAdmin at login time so
// the UI can render admin affordances without an extra round trip. It
// is NOT used for authorization: the adminAuth middleware re-checks the
// admins table on every admin request, so a stale or forged claim can
// never grant elevated access, and a revoked admin's stale cookie still
// hits 403 on the next admin call.
type JWTClaims struct {
	UserID  string
	Email   string
	IsAdmin bool
}

// SignJWT creates an HS256-signed JWT with the given user_id, email,
// admin hint, and expiry. Minimal implementation — no external JWT
// library needed.
func SignJWT(userID, email string, isAdmin bool, secret []byte, ttl time.Duration) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	exp := time.Now().Add(ttl).Unix()
	// Use json.Marshal for the payload to safely escape email (may contain
	// characters that would break a fmt.Sprintf JSON string).
	payloadBytes, _ := json.Marshal(map[string]any{
		"sub":      userID,
		"email":    email,
		"is_admin": isAdmin,
		"exp":      exp,
	})
	payloadEnc := base64.RawURLEncoding.EncodeToString(payloadBytes)

	sigInput := header + "." + payloadEnc
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sigInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return sigInput + "." + sig
}

// VerifyJWT verifies an HS256 JWT and returns its claims.
// Returns nil if invalid or expired.
func VerifyJWT(token string, secret []byte) *JWTClaims {
	parts := splitDot(token)
	if len(parts) != 3 {
		return nil
	}

	// Verify signature
	sigInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sigInput))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(expectedSig)) != 1 {
		return nil
	}

	// Decode payload
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}

	// Parse claims
	var claims struct {
		Sub     string `json:"sub"`
		Email   string `json:"email"`
		IsAdmin bool   `json:"is_admin"`
		Exp     int64  `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil
	}

	if claims.Sub == "" || claims.Exp == 0 {
		return nil
	}

	if time.Now().Unix() > claims.Exp {
		return nil
	}

	return &JWTClaims{UserID: claims.Sub, Email: claims.Email, IsAdmin: claims.IsAdmin}
}

func splitDot(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}
