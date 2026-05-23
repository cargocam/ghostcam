package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cargocam/ghostcam/camera/internal/state"
)

// Identity holds the camera's permanent ed25519 keypair and derived device ID.
// The struct itself lives in internal/state so subpackages (provisioning,
// telemetry, command dispatch) can pass *state.Identity around without
// importing package main; this alias keeps the main-package spelling
// unchanged at call sites here in camera/.
type Identity = state.Identity

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
			DeviceID:   state.DeriveDeviceID(pub),
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
		DeviceID:   state.DeriveDeviceID(pub),
	}, nil
}
