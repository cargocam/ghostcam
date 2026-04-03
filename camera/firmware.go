package camera

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"
)

type firmwareRelease struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
}

type firmwareResponse struct {
	Release *firmwareRelease `json:"release"`
}

// CheckFirmwareUpdate checks the server for a newer firmware version.
// If found, downloads the binary, replaces the current executable, and returns true.
// The caller should exit so systemd restarts with the new binary.
func CheckFirmwareUpdate(ctx context.Context, client *Client) bool {
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

	if err := downloadAndReplace(ctx, resp.Release.DownloadURL); err != nil {
		slog.Error("firmware update failed", "error", err)
		return false
	}

	slog.Info("firmware updated, restarting", "new_version", resp.Release.Version)
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

func downloadAndReplace(ctx context.Context, url string) error {
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

	// Write to temp file next to the current binary
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	tmpPath := execPath + ".new"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("writing firmware: %w", err)
	}

	slog.Info("firmware downloaded", "size_bytes", n, "arch", runtime.GOARCH)

	// Atomic replace
	if err := os.Rename(tmpPath, execPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replacing binary: %w", err)
	}

	return nil
}
