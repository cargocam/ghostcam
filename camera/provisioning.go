package camera

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// RunProvisioning provisions the camera via a one-time token. Token and server
// URL are resolved in order: CLI/env (cfg.ProvisionToken, cfg.ServerURL) →
// flat files ({dataDir}/provision_token, {dataDir}/server_url).
// Returns nil credentials if no token is available.
func RunProvisioning(ctx context.Context, cfg *CameraConfig, deviceSerial string) (*Credentials, error) {
	token, serverURL := resolveProvisionInputs(cfg)
	if token == "" || serverURL == "" {
		slog.Info("no provision token available, waiting for provisioning")
		return nil, nil
	}

	// Ensure server_url is a full URL
	if !strings.HasPrefix(serverURL, "http://") && !strings.HasPrefix(serverURL, "https://") {
		serverURL = "https://" + serverURL
	}

	slog.Info("attempting provisioning", "server", serverURL)

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

	if err := SaveCredentials(cfg.DataDir, creds); err != nil {
		return nil, err
	}

	// Remove the one-time token file (if it exists)
	_ = os.Remove(filepath.Join(cfg.DataDir, "provision_token"))

	slog.Info("provisioning complete", "device_id", creds.DeviceID)
	return creds, nil
}

// resolveProvisionInputs returns (token, serverURL) from CLI/env first, then flat files.
func resolveProvisionInputs(cfg *CameraConfig) (string, string) {
	if cfg.ProvisionToken != "" && cfg.ServerURL != "" {
		return cfg.ProvisionToken, cfg.ServerURL
	}

	token := readTrimmedFile(filepath.Join(cfg.DataDir, "provision_token"))
	serverURL := readTrimmedFile(filepath.Join(cfg.DataDir, "server_url"))
	if token != "" && serverURL != "" {
		return token, serverURL
	}

	return "", ""
}
