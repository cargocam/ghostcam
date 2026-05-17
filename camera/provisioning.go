package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cargocam/ghostcam/common"
)

// RunProvisioning provisions the camera via a one-time token. Token and
// server URL are resolved in order:
//
//   1. CLI/env (cfg.ProvisionToken, cfg.ServerURL)
//   2. Flat files ({dataDir}/provision_token, {dataDir}/server_url)
//   3. QR scan via rpicam-still AND BLE GATT peripheral, raced — first
//      payload wins, loser's context is cancelled. 5-min shared timeout.
//
// The camera sends its ed25519 public key to the server — no secret is
// returned. Returns nil credentials if no token is available from any source.
func RunProvisioning(ctx context.Context, cfg *CameraConfig, deviceSerial string, identity *Identity) (*Credentials, error) {
	token, serverURL := resolveProvisionInputs(cfg)

	// Fallback: race QR scan + BLE GATT peripheral.
	if token == "" || serverURL == "" {
		payload, err := raceQRandBT(ctx, identity.DeviceID)
		if err != nil {
			return nil, err
		}
		if payload == nil {
			slog.Info("no provision token available, waiting for provisioning")
			return nil, nil
		}
		token = payload.Token
		serverURL = payload.Server

		if payload.WifiSSID != "" {
			psk := payload.WifiPassword
			if err := EnsureWifi(ctx, payload.WifiSSID, &psk); err != nil {
				slog.Warn("WiFi from onboarding payload failed", "ssid", payload.WifiSSID, "err", err)
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

// raceQRandBT runs ScanQR and ScanBT concurrently and returns the first
// payload that arrives, cancelling the loser. nil result + nil error
// means both timed out without a payload.
func raceQRandBT(ctx context.Context, deviceID string) (*common.QRPayload, error) {
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		payload *common.QRPayload
		err     error
		source  string
	}
	ch := make(chan result, 2)

	go func() {
		p, err := ScanQR(raceCtx)
		ch <- result{p, err, "qr"}
	}()
	go func() {
		// Device-ID prefix used in the BT advertised name so users see
		// e.g. "Ghostcam-45a8b310" and can disambiguate when multiple
		// un-onboarded Pis are nearby.
		prefix := deviceID
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		p, err := ScanBT(raceCtx, prefix)
		ch <- result{p, err, "bt"}
	}()

	// Wait for either a payload or both to finish empty.
	var lastErr error
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.payload != nil {
			slog.Info("onboarding payload accepted", "source", r.source)
			cancel()
			// Drain the second goroutine in the background so it can exit
			// cleanly on its own; don't block the caller.
			go func() { <-ch }()
			return r.payload, nil
		}
		if r.err != nil && !errors.Is(r.err, context.Canceled) {
			lastErr = fmt.Errorf("%s: %w", r.source, r.err)
		}
	}
	return nil, lastErr
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
