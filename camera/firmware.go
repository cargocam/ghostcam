package main

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

type firmwareRelease struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	SHA256      string `json:"sha256"`
}

type firmwareResponse struct {
	Release *firmwareRelease `json:"release"`
}

// CheckFirmwareUpdate checks the server for a newer firmware version.
// If found, downloads the binary to {dataDir}/staged-update.deb, and returns true.
// The caller should exit so systemd restarts. ExecStartPre backs up the
// current binary and installs the staged update; if the new binary fails
// to write boot_ok (crash-loop), the next restart rolls back to .prev.
func CheckFirmwareUpdate(ctx context.Context, client *Client, dataDir string) bool {
	if Version == "dev" {
		slog.Debug("firmware check skipped (dev build)")
		return false
	}

	slog.Info("checking for firmware update", "current_version", Version)

	resp, err := client.getFirmwareLatest(ctx)
	if err != nil {
		slog.Warn("firmware check failed", "error", err)
		return false
	}

	if resp.Release == nil {
		slog.Debug("no firmware release available")
		return false
	}

	if resp.Release.Version == Version {
		slog.Debug("firmware is up to date", "version", Version)
		return false
	}

	slog.Info("new firmware available", "current", Version, "new", resp.Release.Version)

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

func (c *Client) getFirmwareLatest(ctx context.Context) (*firmwareResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url := fmt.Sprintf("%s/api/v1/firmware/latest", c.serverURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
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
