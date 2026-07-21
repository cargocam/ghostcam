package bt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cargocam/ghostcam/camera/internal/network"
	"github.com/cargocam/ghostcam/camera/internal/state"
	"github.com/cargocam/ghostcam/common"
)

// Provisioner abstracts the HTTPS call that registers the camera's
// ed25519 public key with the server. The concrete implementation lives
// in camera/client.go (package main) — it can't be referenced from
// here because internal subpackages can't import main, so RunProvisioning
// takes the function as a parameter and main wires it up at the call
// site.
type Provisioner func(ctx context.Context, serverURL, token, deviceSerial string, identity *state.Identity) error

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
func RunProvisioning(ctx context.Context, cfg *state.CameraConfig, deviceSerial string, identity *state.Identity, provision Provisioner) (*state.Credentials, error) {
	token, serverURL := resolveProvisionInputs(cfg)

	// Fallback: race QR scan + BLE GATT peripheral + local HTTP server.
	if token == "" || serverURL == "" {
		payload, err := raceQRandBT(ctx, identity.DeviceID, cfg.ProvisionHTTPAddr)
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
			if err := network.EnsureWifi(ctx, payload.WifiSSID, &psk); err != nil {
				slog.Warn("WiFi from onboarding payload failed", "ssid", payload.WifiSSID, "err", err)
			}
			network.WaitForRoute(ctx)
		}

		if payload.CellularAPN != "" {
			state.PersistCellular(cfg.DataDir, payload.CellularAPN, payload.CellularUser, payload.CellularPass)
			if err := network.EnsureCellular(ctx, payload.CellularAPN, payload.CellularUser, payload.CellularPass); err != nil {
				slog.Warn("cellular from onboarding payload failed", "apn", payload.CellularAPN, "err", err)
			}
			network.WaitForRoute(ctx)
		}
	}

	// Ensure server_url is a full URL
	if !strings.HasPrefix(serverURL, "http://") && !strings.HasPrefix(serverURL, "https://") {
		serverURL = "https://" + serverURL
	}

	slog.Info("attempting provisioning", "server", serverURL)

	if err := provision(ctx, serverURL, token, deviceSerial, identity); err != nil {
		return nil, err
	}

	creds := &state.Credentials{
		DeviceID:  identity.DeviceID,
		ServerURL: serverURL,
		Identity:  identity,
	}

	// Persist server_url under dataDir. Identity files are managed by
	// LoadOrCreateIdentity (package main) and intentionally not written
	// here so a re-provisioning cycle preserves the keypair.
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "server_url"), []byte(creds.ServerURL), 0600); err != nil {
		return nil, fmt.Errorf("writing server_url: %w", err)
	}

	// Remove the one-time token file (if it exists)
	_ = os.Remove(filepath.Join(cfg.DataDir, "provision_token"))

	slog.Info("provisioning complete", "device_id", creds.DeviceID)
	return creds, nil
}

// raceQRandBT runs the onboarding channels — QR scan, BLE GATT peripheral,
// and the local offline HTTP server — concurrently and returns the first
// payload that arrives, cancelling the losers. nil result + nil error
// means every channel finished empty (all timed out / none configured).
//
// httpAddr is the bind address for the local HTTP channel (USB gadget /
// SoftAP); empty makes that channel a no-op, so on hardware without the
// gadget link this behaves exactly like the prior QR+BT race.
func raceQRandBT(ctx context.Context, deviceID, httpAddr string) (*common.QRPayload, error) {
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		payload *common.QRPayload
		err     error
		source  string
	}
	const channels = 3
	ch := make(chan result, channels)

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
	go func() {
		p, err := ScanLocalHTTP(raceCtx, deviceID, httpAddr)
		ch <- result{p, err, "http"}
	}()

	// Wait for either a payload or all channels to finish empty.
	var lastErr error
	for i := 0; i < channels; i++ {
		r := <-ch
		if r.payload != nil {
			slog.Info("onboarding payload accepted", "source", r.source)
			cancel()
			// Losers get cancelled via raceCtx and their sends land in the
			// buffered channel (cap == channel count) without blocking, so
			// they exit cleanly on their own — nothing to drain, no leak.
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
func resolveProvisionInputs(cfg *state.CameraConfig) (string, string) {
	token := cfg.ProvisionToken
	serverURL := cfg.ServerURL

	if token == "" {
		token = state.ReadTrimmedFile(filepath.Join(cfg.DataDir, "provision_token"))
	}
	if serverURL == "" {
		serverURL = state.ReadTrimmedFile(filepath.Join(cfg.DataDir, "server_url"))
	}

	if token != "" && serverURL != "" {
		return token, serverURL
	}
	return "", ""
}
