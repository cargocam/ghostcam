package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Identity holds the camera's permanent ed25519 keypair and derived device ID.
// Generated on first boot, never regenerated — survives server switches.
type Identity struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
	DeviceID   string // SHA-256(public_key)[:16] hex, 32 chars
}

// LoadOrCreateIdentity loads the ed25519 keypair from dataDir, or generates
// one on first boot. The keypair is permanent camera identity — like
// ~/.ssh/id_ed25519 — and is never cleared by ClearCredentials.
func LoadOrCreateIdentity(dataDir string) (*Identity, error) {
	seedPath := filepath.Join(dataDir, "identity_key")
	pubPath := filepath.Join(dataDir, "identity_key.pub")

	seedHex := readTrimmedFile(seedPath)
	if seedHex != "" {
		seed, err := hex.DecodeString(seedHex)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("corrupt identity_key")
		}
		priv := ed25519.NewKeyFromSeed(seed)
		pub := priv.Public().(ed25519.PublicKey)
		return &Identity{
			PrivateKey: priv,
			PublicKey:  pub,
			DeviceID:   deriveDeviceID(pub),
		}, nil
	}

	// Generate new keypair on first boot.
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("generating keypair: %w", err)
	}

	if err := os.WriteFile(seedPath, []byte(hex.EncodeToString(priv.Seed())), 0600); err != nil {
		return nil, fmt.Errorf("writing identity_key: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(hex.EncodeToString(pub)), 0644); err != nil {
		return nil, fmt.Errorf("writing identity_key.pub: %w", err)
	}

	return &Identity{
		PrivateKey: priv,
		PublicKey:  pub,
		DeviceID:   deriveDeviceID(pub),
	}, nil
}

// PublicKeyHex returns the hex-encoded public key for transmission to the server.
func (id *Identity) PublicKeyHex() string {
	return hex.EncodeToString(id.PublicKey)
}

// deriveDeviceID returns the first 16 bytes of SHA-256(publicKey) as hex (32 chars).
func deriveDeviceID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:16])
}
