package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cargocam/ghostcam/camera/internal/state"
)

// Credentials holds the persisted camera identity and server binding.
// The struct itself lives in internal/state so subpackages (provisioning
// under internal/bt, command dispatch under internal/commands) can
// reference it without importing package main; this alias keeps the
// main-package spelling unchanged at call sites here in camera/.
type Credentials = state.Credentials

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
// mode on next startup. Delegates to state so the same logic is
// reachable from internal/commands (which can't import main).
func ClearCredentials(dataDir string) {
	state.ClearCredentials(dataDir)
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

// readTrimmedFile reads path, trims surrounding whitespace, and returns
// the contents — or "" on any error. Kept here as a package-main
// helper so identity.go / credentials.go don't have to reach into the
// internal/state copy at every call site.
func readTrimmedFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
