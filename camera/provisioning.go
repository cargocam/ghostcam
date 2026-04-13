package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// RunProvisioning provisions the camera via a one-time token. Token and server
// URL are resolved in order: CLI/env (cfg.ProvisionToken, cfg.ServerURL) →
// flat files ({dataDir}/provision_token, {dataDir}/server_url) →
// QR code scan via rpicam-still (Linux only, 5-min timeout).
//
// The camera sends its ed25519 public key to the server — no secret is
// returned. Returns nil credentials if no token is available from any source.
func RunProvisioning(ctx context.Context, cfg *CameraConfig, deviceSerial string, identity *Identity) (*Credentials, error) {
	token, serverURL := resolveProvisionInputs(cfg)

	// Fallback: scan QR code from camera
	if token == "" || serverURL == "" {
		qr, err := ScanQR(ctx)
		if err != nil {
			return nil, fmt.Errorf("QR scan: %w", err)
		}
		if qr == nil {
			slog.Info("no provision token available, waiting for provisioning")
			return nil, nil
		}
		token = qr.Token
		serverURL = qr.Server

		// QR may include WiFi credentials
		if qr.WifiSSID != "" {
			psk := qr.WifiPassword
			if err := EnsureWifi(ctx, qr.WifiSSID, &psk); err != nil {
				slog.Warn("WiFi from QR failed", "ssid", qr.WifiSSID, "err", err)
			}
			WaitForRoute(ctx)
		}
	}

	// Ensure server_url is a full URL
	if !strings.HasPrefix(serverURL, "http://") && !strings.HasPrefix(serverURL, "https://") {
		serverURL = "https://" + serverURL
	}

	slog.Info("attempting provisioning", "server", serverURL)

	if err := Provision(ctx, serverURL, token, deviceSerial, identity); err != nil {
		return nil, err
	}

	creds := &Credentials{
		DeviceID:  identity.DeviceID,
		ServerURL: serverURL,
		Identity:  identity,
	}

	if err := SaveCredentials(cfg.DataDir, creds); err != nil {
		return nil, err
	}

	// Remove the one-time token file (if it exists)
	_ = os.Remove(filepath.Join(cfg.DataDir, "provision_token"))

	slog.Info("provisioning complete", "device_id", creds.DeviceID)
	return creds, nil
}

// resolveProvisionInputs returns (token, serverURL) by merging CLI/env
// and flat file sources. Either source can provide either value.
func resolveProvisionInputs(cfg *CameraConfig) (string, string) {
	token := cfg.ProvisionToken
	serverURL := cfg.ServerURL

	if token == "" {
		token = readTrimmedFile(filepath.Join(cfg.DataDir, "provision_token"))
	}
	if serverURL == "" {
		serverURL = readTrimmedFile(filepath.Join(cfg.DataDir, "server_url"))
	}

	if token != "" && serverURL != "" {
		return token, serverURL
	}
	return "", ""
}
