package camera

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Credentials holds the persisted camera identity.
type Credentials struct {
	APIKey    string
	DeviceID  string
	ServerURL string
}

// LoadCredentials reads api_key, device_id, and server_url from flat files in dataDir.
// Returns nil if any file is missing or empty.
func LoadCredentials(dataDir string) *Credentials {
	apiKey := readTrimmedFile(filepath.Join(dataDir, "api_key"))
	deviceID := readTrimmedFile(filepath.Join(dataDir, "device_id"))
	serverURL := readTrimmedFile(filepath.Join(dataDir, "server_url"))

	if apiKey == "" || deviceID == "" || serverURL == "" {
		return nil
	}

	return &Credentials{
		APIKey:    apiKey,
		DeviceID:  deviceID,
		ServerURL: serverURL,
	}
}

// SaveCredentials writes api_key, device_id, and server_url to flat files in dataDir.
func SaveCredentials(dataDir string, creds *Credentials) error {
	if err := os.WriteFile(filepath.Join(dataDir, "api_key"), []byte(creds.APIKey), 0600); err != nil {
		return fmt.Errorf("writing api_key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "device_id"), []byte(creds.DeviceID), 0644); err != nil {
		return fmt.Errorf("writing device_id: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "server_url"), []byte(creds.ServerURL), 0644); err != nil {
		return fmt.Errorf("writing server_url: %w", err)
	}
	return nil
}

func readTrimmedFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
