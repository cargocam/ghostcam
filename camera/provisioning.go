package camera

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// RunProvisioning checks for a pre-provisioned token file, calls Provision,
// saves credentials, and returns them. Returns nil credentials if provisioning
// fails or no token is available.
func RunProvisioning(ctx context.Context, dataDir, deviceSerial string) (*Credentials, error) {
	tokenPath := filepath.Join(dataDir, "provision_token")
	serverURLPath := filepath.Join(dataDir, "server_url")

	tokenData, err := os.ReadFile(tokenPath)
	if err != nil {
		slog.Info("no provision_token file found, waiting for provisioning")
		return nil, nil
	}
	serverURLData, err := os.ReadFile(serverURLPath)
	if err != nil {
		slog.Info("no server_url file found, waiting for provisioning")
		return nil, nil
	}

	token := strings.TrimSpace(string(tokenData))
	serverURL := strings.TrimSpace(string(serverURLData))

	if token == "" || serverURL == "" {
		slog.Info("provision_token or server_url empty")
		return nil, nil
	}

	// Ensure server_url is a full URL
	if !strings.HasPrefix(serverURL, "http://") && !strings.HasPrefix(serverURL, "https://") {
		serverURL = "https://" + serverURL
	}

	slog.Info("found pre-provisioned token, attempting provisioning", "server", serverURL)

	resp, err := Provision(ctx, serverURL, token, deviceSerial)
	if err != nil {
		slog.Warn("provisioning failed", "err", err)
		return nil, err
	}

	creds := &Credentials{
		APIKey:    resp.APIKey,
		DeviceID:  resp.DeviceID,
		ServerURL: serverURL,
	}

	if err := SaveCredentials(dataDir, creds); err != nil {
		return nil, err
	}

	// Remove the one-time token file
	_ = os.Remove(tokenPath)

	slog.Info("provisioning complete", "device_id", creds.DeviceID)
	return creds, nil
}
