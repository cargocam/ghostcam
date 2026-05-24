package firmware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Client is the firmware updater's narrow view of the server HTTP
// client. The concrete *main.Client (camera/client.go) satisfies this
// surface; defining the interface here lets firmware live in its own
// subpackage without importing package main.
type Client interface {
	// HTTPClient returns the underlying *http.Client; firmware uses it
	// directly for the (currently unauthenticated) GET /firmware/latest
	// call and the .deb download.
	HTTPClient() *http.Client
	// ServerURL returns the trimmed server base URL (no trailing slash).
	ServerURL() string
}

type firmwareRelease struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	SHA256      string `json:"sha256"`
}

type firmwareResponse struct {
	Release *firmwareRelease `json:"release"`
}

// CheckFirmwareUpdate checks the server for a newer firmware version.
// If found, downloads the .deb to {dataDir}/staged-update.deb and returns
// true. The caller should exit so systemd restarts. ExecStartPre backs up
// the current binary and runs dpkg -i; if the new binary fails to write
// boot_ok (crash-loop), the next restart rolls back to .prev.
//
// Invoked only via the telemetry-driven `update_firmware` command path
// (camera/commands.go) — never at startup. The eager startup call was
// removed in f87a0bf because boot_ok is written after first telemetry
// success, so an eager exit at boot raced the rollback gate.
//
// version is the running binary's main.Version. Passed as a parameter
// (rather than read from package main directly) because internal
// subpackages can't import main, and the -X main.Version=$SHA
// reproducible-build gate requires Version to stay there.
func CheckFirmwareUpdate(ctx context.Context, client Client, dataDir, version string) bool {
	if version == "dev" {
		slog.Debug("firmware check skipped (dev build)")
		return false
	}

	slog.Info("checking for firmware update", "current_version", version)

	resp, err := getFirmwareLatest(ctx, client)
	if err != nil {
		slog.Warn("firmware check failed", "error", err)
		return false
	}

	if resp.Release == nil {
		slog.Debug("no firmware release available")
		return false
	}

	if resp.Release.Version == version {
		slog.Debug("firmware is up to date", "version", version)
		return false
	}

	slog.Info("new firmware available", "current", version, "new", resp.Release.Version)

	stagedPath := filepath.Join(dataDir, "staged-update.deb")
	if err := downloadToFile(ctx, resp.Release.DownloadURL, stagedPath); err != nil {
		slog.Error("firmware download failed", "error", err)
		return false
	}

	// Verify SHA256 if server provided a hash (backward-compat: skip if empty)
	if resp.Release.SHA256 != "" {
		actual, err := fileHash(stagedPath)
		if err != nil {
			slog.Error("firmware hash computation failed", "error", err)
			os.Remove(stagedPath)
			return false
		}
		if actual != resp.Release.SHA256 {
			slog.Error("firmware hash mismatch, discarding",
				"expected", resp.Release.SHA256, "actual", actual)
			os.Remove(stagedPath)
			return false
		}
		slog.Info("firmware hash verified", "sha256", actual)
	}

	slog.Info("firmware staged, restarting for install", "new_version", resp.Release.Version, "staged", stagedPath)
	return true
}

func getFirmwareLatest(ctx context.Context, client Client) (*firmwareResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url := fmt.Sprintf("%s/api/v1/firmware/latest", client.ServerURL())
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("firmware check returned %d", resp.StatusCode)
	}

	var result firmwareResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding firmware response: %w", err)
	}
	return &result, nil
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func downloadToFile(ctx context.Context, url, destPath string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading firmware: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	tmpPath := destPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("writing firmware: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("staging firmware: %w", err)
	}

	slog.Info("firmware downloaded", "size_bytes", n, "path", destPath)
	return nil
}
