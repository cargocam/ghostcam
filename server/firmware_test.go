package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifyGithubSignature(t *testing.T) {
	secret := "test-secret-123"
	body := []byte(`{"action":"published","release":{"tag_name":"v0.5.0"}}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validHeader := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name   string
		header string
		body   []byte
		secret string
		want   bool
	}{
		{"valid signature", validHeader, body, secret, true},
		{"empty header", "", body, secret, false},
		{"missing prefix", hex.EncodeToString(mac.Sum(nil)), body, secret, false},
		{"wrong prefix", "sha1=" + hex.EncodeToString(mac.Sum(nil)), body, secret, false},
		{"not hex", "sha256=notahexstring!!", body, secret, false},
		{"tampered body", validHeader, []byte(`{"action":"edited"}`), secret, false},
		{"wrong secret", validHeader, body, "different-secret", false},
		{"empty secret rejects real sig", validHeader, body, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := verifyGithubSignature(tc.header, tc.body, tc.secret)
			if got != tc.want {
				t.Errorf("verifyGithubSignature(%q) = %v; want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestPiImageAssetRegex(t *testing.T) {
	tests := []struct {
		name       string
		want       bool
		wantDevice string
		wantVer    string
	}{
		{"ghostcam-zero2w-v0.5.0.img.xz", true, "zero2w", "v0.5.0"},
		{"ghostcam-pi4-v1.2.3.img.xz", true, "pi4", "v1.2.3"},
		{"ghostcam-pi5-v0.5.0.img.xz", true, "pi5", "v0.5.0"},
		{"ghostcam-camera-aarch64", false, "", ""},
		{"ghostcam-pi6-v0.5.0.img.xz", false, "", ""},
		{"ghostcam-pi4-v0.5.0.img", false, "", ""},
		{"prefix-ghostcam-pi4-v0.5.0.img.xz", false, "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := piImageAssetRe.FindStringSubmatch(tc.name)
			if tc.want {
				if m == nil {
					t.Fatalf("expected match, got nil")
				}
				if m[1] != tc.wantDevice {
					t.Errorf("device = %q; want %q", m[1], tc.wantDevice)
				}
				if m[2] != tc.wantVer {
					t.Errorf("version = %q; want %q", m[2], tc.wantVer)
				}
			} else if m != nil {
				t.Errorf("expected no match, got %v", m)
			}
		})
	}
}
