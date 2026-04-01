//go:build linux

package camera

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// EnsureWifi connects to the given WiFi network via nmcli if not already connected.
func EnsureWifi(ctx context.Context, ssid string, psk *string) error {
	// Check if already connected
	out, err := exec.CommandContext(ctx, "nmcli", "connection", "show", "--active").Output()
	if err != nil {
		slog.Warn("nmcli not available", "err", err)
		return nil
	}
	if strings.Contains(string(out), ssid) {
		slog.Debug("already connected to WiFi", "ssid", ssid)
		return nil
	}

	slog.Info("connecting to WiFi network", "ssid", ssid)

	args := []string{"device", "wifi", "connect", ssid}
	if psk != nil {
		args = append(args, "password", *psk)
	}

	cmd := exec.CommandContext(ctx, "nmcli", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("WiFi connection failed: %s", strings.TrimSpace(string(output)))
	}

	slog.Info("WiFi connected", "ssid", ssid)
	return nil
}

// WaitForRoute blocks until a default route exists in /proc/net/route.
func WaitForRoute(ctx context.Context) {
	if readDefaultInterface() != "" {
		return
	}
	slog.Info("no default route, waiting for network...")
	start := time.Now()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if readDefaultInterface() != "" {
				slog.Info("default route appeared", "elapsed_s", time.Since(start).Seconds())
				return
			}
		}
	}
}

// readDefaultInterface reads the default route interface from /proc/net/route.
func readDefaultInterface() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	return parseDefaultInterface(string(data))
}

// parseDefaultInterface extracts the default route interface from /proc/net/route content.
func parseDefaultInterface(content string) string {
	for i, line := range strings.Split(content, "\n") {
		if i == 0 {
			continue // skip header
		}
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 && fields[1] == "00000000" {
			return fields[0]
		}
	}
	return ""
}
