package auth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// MaxTimestampSkew is the maximum allowed clock skew between camera and
// server for signature authentication. 5 minutes is generous enough for
// Pi clock drift but tight enough to limit replay windows.
const MaxTimestampSkew = 300 // seconds

// SignatureAuth holds the parsed fields from a Signature authorization header.
type SignatureAuth struct {
	DeviceID  string
	Timestamp int64
	Signature []byte
}

// ParseSignatureHeader parses "Signature device_id=...,ts=...,sig=..." into
// its components. Returns an error if the header is malformed.
func ParseSignatureHeader(header string) (*SignatureAuth, error) {
	val, ok := strings.CutPrefix(header, "Signature ")
	if !ok {
		return nil, fmt.Errorf("not a Signature header")
	}

	fields := make(map[string]string)
	for _, part := range strings.Split(val, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("malformed field: %s", part)
		}
		fields[k] = v
	}

	deviceID := fields["device_id"]
	tsStr := fields["ts"]
	sigB64 := fields["sig"]
	if deviceID == "" || tsStr == "" || sigB64 == "" {
		return nil, fmt.Errorf("missing required fields")
	}

	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp: %w", err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, fmt.Errorf("invalid signature encoding: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("invalid signature length: got %d, want %d", len(sig), ed25519.SignatureSize)
	}

	return &SignatureAuth{DeviceID: deviceID, Timestamp: ts, Signature: sig}, nil
}

// VerifySignature checks the ed25519 signature against the reconstructed
// message (METHOD\nPATH\nTIMESTAMP\nDEVICE_ID) and validates timestamp
// freshness.
func VerifySignature(sa *SignatureAuth, method, path string, publicKeyHex string) bool {
	now := time.Now().Unix()
	if math.Abs(float64(now-sa.Timestamp)) > MaxTimestampSkew {
		return false
	}

	pubBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return false
	}

	message := fmt.Sprintf("%s\n%s\n%d\n%s", method, path, sa.Timestamp, sa.DeviceID)
	return ed25519.Verify(ed25519.PublicKey(pubBytes), []byte(message), sa.Signature)
}

// DeriveDeviceID returns the first 16 bytes of SHA-256(publicKey) as hex
// (32 chars). This makes the device_id a fingerprint of the key — stable
// forever, unique across servers, and deterministic from the keypair.
func DeriveDeviceID(publicKey []byte) string {
	h := sha256.Sum256(publicKey)
	return hex.EncodeToString(h[:16])
}
