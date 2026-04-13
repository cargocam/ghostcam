package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Credentials holds the persisted camera identity and server binding.
type Credentials struct {
	DeviceID  string
	ServerURL string
	Identity  *Identity
}

// LoadCredentials reads camera credentials from flat files in dataDir.
// Returns nil if identity_key or server_url is missing (needs provisioning).
func LoadCredentials(dataDir string) *Credentials {
	identity := loadIdentityIfExists(dataDir)
	if identity == nil {
		return nil
	}

	serverURL := readTrimmedFile(filepath.Join(dataDir, "server_url"))
	if serverURL == "" {
		return nil
	}

	return &Credentials{
		DeviceID:  identity.DeviceID,
		ServerURL: serverURL,
		Identity:  identity,
	}
}

// SaveCredentials writes server_url to dataDir. Identity files are
// managed by LoadOrCreateIdentity and are not written here.
func SaveCredentials(dataDir string, creds *Credentials) error {
	if err := os.WriteFile(filepath.Join(dataDir, "server_url"), []byte(creds.ServerURL), 0600); err != nil {
		return fmt.Errorf("writing server_url: %w", err)
	}
	return nil
}

// ClearCredentials removes server binding but preserves the camera's
// permanent identity (keypair). The camera will re-enter provisioning
// mode on next startup.
func ClearCredentials(dataDir string) {
	_ = os.Remove(filepath.Join(dataDir, "server_url"))
	// identity_key and identity_key.pub are NEVER removed.
}

// loadIdentityIfExists reads an existing keypair from dataDir without
// creating one. Returns nil if identity_key doesn't exist.
func loadIdentityIfExists(dataDir string) *Identity {
	seedHex := readTrimmedFile(filepath.Join(dataDir, "identity_key"))
	if seedHex == "" {
		return nil
	}
	identity, err := LoadOrCreateIdentity(dataDir)
	if err != nil {
		return nil
	}
	return identity
}

func readTrimmedFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
