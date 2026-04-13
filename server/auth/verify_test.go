package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func TestParseSignatureHeader(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	deviceID := DeriveDeviceID(pub)
	ts := time.Now().Unix()
	message := fmt.Sprintf("POST\n/api/v1/cameras/%s/telemetry\n%d\n%s", deviceID, ts, deviceID)
	sig := ed25519.Sign(priv, []byte(message))
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	header := fmt.Sprintf("Signature device_id=%s,ts=%d,sig=%s", deviceID, ts, sigB64)

	sa, err := ParseSignatureHeader(header)
	if err != nil {
		t.Fatalf("ParseSignatureHeader: %v", err)
	}
	if sa.DeviceID != deviceID {
		t.Errorf("DeviceID = %q, want %q", sa.DeviceID, deviceID)
	}
	if sa.Timestamp != ts {
		t.Errorf("Timestamp = %d, want %d", sa.Timestamp, ts)
	}
}

func TestParseSignatureHeader_BadPrefix(t *testing.T) {
	_, err := ParseSignatureHeader("Bearer sometoken")
	if err == nil {
		t.Error("expected error for Bearer prefix")
	}
}

func TestParseSignatureHeader_MissingFields(t *testing.T) {
	_, err := ParseSignatureHeader("Signature device_id=abc,ts=123")
	if err == nil {
		t.Error("expected error for missing sig field")
	}
}

func TestVerifySignature_RoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	deviceID := DeriveDeviceID(pub)
	pubHex := hex.EncodeToString(pub)

	ts := time.Now().Unix()
	method := "POST"
	path := "/api/v1/cameras/" + deviceID + "/telemetry"
	message := fmt.Sprintf("%s\n%s\n%d\n%s", method, path, ts, deviceID)
	sig := ed25519.Sign(priv, []byte(message))

	sa := &SignatureAuth{
		DeviceID:  deviceID,
		Timestamp: ts,
		Signature: sig,
	}

	if !VerifySignature(sa, method, path, pubHex) {
		t.Error("VerifySignature returned false for valid signature")
	}
}

func TestVerifySignature_ExpiredTimestamp(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	deviceID := DeriveDeviceID(pub)
	pubHex := hex.EncodeToString(pub)

	ts := time.Now().Unix() - MaxTimestampSkew - 10 // expired
	method := "POST"
	path := "/api/v1/cameras/" + deviceID + "/telemetry"
	message := fmt.Sprintf("%s\n%s\n%d\n%s", method, path, ts, deviceID)
	sig := ed25519.Sign(priv, []byte(message))

	sa := &SignatureAuth{
		DeviceID:  deviceID,
		Timestamp: ts,
		Signature: sig,
	}

	if VerifySignature(sa, method, path, pubHex) {
		t.Error("VerifySignature should reject expired timestamp")
	}
}

func TestVerifySignature_WrongKey(t *testing.T) {
	pub1, _, _ := ed25519.GenerateKey(nil)
	_, priv2, _ := ed25519.GenerateKey(nil)
	deviceID := DeriveDeviceID(pub1)
	pubHex := hex.EncodeToString(pub1)

	ts := time.Now().Unix()
	method := "POST"
	path := "/test"
	message := fmt.Sprintf("%s\n%s\n%d\n%s", method, path, ts, deviceID)
	sig := ed25519.Sign(priv2, []byte(message)) // signed with wrong key

	sa := &SignatureAuth{
		DeviceID:  deviceID,
		Timestamp: ts,
		Signature: sig,
	}

	if VerifySignature(sa, method, path, pubHex) {
		t.Error("VerifySignature should reject signature from wrong key")
	}
}

func TestVerifySignature_WrongPath(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	deviceID := DeriveDeviceID(pub)
	pubHex := hex.EncodeToString(pub)

	ts := time.Now().Unix()
	message := fmt.Sprintf("POST\n/correct-path\n%d\n%s", ts, deviceID)
	sig := ed25519.Sign(priv, []byte(message))

	sa := &SignatureAuth{
		DeviceID:  deviceID,
		Timestamp: ts,
		Signature: sig,
	}

	if VerifySignature(sa, "POST", "/wrong-path", pubHex) {
		t.Error("VerifySignature should reject mismatched path")
	}
}

func TestDeriveDeviceID_Deterministic(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	id1 := DeriveDeviceID(pub)
	id2 := DeriveDeviceID(pub)
	if id1 != id2 {
		t.Errorf("DeriveDeviceID not deterministic: %q != %q", id1, id2)
	}
	if len(id1) != 32 {
		t.Errorf("DeriveDeviceID length = %d, want 32", len(id1))
	}
}

func TestDeriveDeviceID_Unique(t *testing.T) {
	pub1, _, _ := ed25519.GenerateKey(nil)
	pub2, _, _ := ed25519.GenerateKey(nil)
	if DeriveDeviceID(pub1) == DeriveDeviceID(pub2) {
		t.Error("DeriveDeviceID collision for different keys")
	}
}
