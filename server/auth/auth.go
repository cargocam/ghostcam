// Package auth provides password hashing, HMAC token signing, and session ID generation.
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

// GenerateSessionID returns a cryptographically random session ID (32 bytes, URL-safe base64).
func GenerateSessionID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
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

// SignJWT creates an HS256-signed JWT with the given user_id and expiry.
// Minimal implementation — no external JWT library needed.
func SignJWT(userID string, secret []byte, ttl time.Duration) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	exp := time.Now().Add(ttl).Unix()
	payload := fmt.Sprintf(`{"sub":"%s","exp":%d}`, userID, exp)
	payloadEnc := base64.RawURLEncoding.EncodeToString([]byte(payload))

	sigInput := header + "." + payloadEnc
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sigInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return sigInput + "." + sig
}

// VerifyJWT verifies an HS256 JWT and returns the user_id (sub claim).
// Returns empty string if invalid or expired.
func VerifyJWT(token string, secret []byte) string {
	parts := splitDot(token)
	if len(parts) != 3 {
		return ""
	}

	// Verify signature
	sigInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sigInput))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(expectedSig)) != 1 {
		return ""
	}

	// Decode payload
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}

	// Parse claims
	var claims struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return ""
	}

	if claims.Sub == "" || claims.Exp == 0 {
		return ""
	}

	if time.Now().Unix() > claims.Exp {
		return ""
	}

	return claims.Sub
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
