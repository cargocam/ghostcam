//go:build linux

package network

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

	// Disable the 4-retry autoconnect cap on the just-created connection.
	// NM defaults to 4 retries before marking a connection "blocked"
	// permanently — fine for desktop users with a tray icon to retry,
	// fatal for a headless camera. A single WPA group-key rekey failure
	// or transient AP blip otherwise leaves the Pi off-network until
	// reboot. `nmcli device wifi connect` doesn't accept this property
	// at create time, so we modify the resulting connection by ID.
	// Best-effort: log a warn on failure but don't fail the onboarding
	// — wifi is up either way.
	modCmd := exec.CommandContext(ctx, "nmcli", "connection", "modify", ssid,
		"connection.autoconnect-retries", "0")
	if modOut, modErr := modCmd.CombinedOutput(); modErr != nil {
		slog.Warn("failed to set infinite-retry on wifi connection",
			"ssid", ssid, "err", modErr,
			"output", strings.TrimSpace(string(modOut)))
	}

	slog.Info("WiFi connected", "ssid", ssid)
	return nil
}

// EnsureCellular provisions a NetworkManager `gsm` connection for the
// SIM7600 data bearer when an APN is configured. Nothing else in the
// stack creates one, so a SIM whose APN isn't in ModemManager's provider
// database enables the modem but never connects — the camera then has no
// cellular uplink even though the hardware is fine. This is the missing
// piece behind "cellular doesn't work" on such SIMs.
//
// Behaviour:
//   - apn == "": no-op (leave cellular to MM/NM auto-config).
//   - our connection already exists: modify it in place (keeps APN/creds
//     in sync if the operator changed them).
//   - some *other* gsm connection already exists: leave it untouched —
//     the image or operator configured cellular deliberately; we don't
//     clobber it.
//   - none exists: create ours (autoconnect on, infinite retries like
//     EnsureWifi so a transient blip can't permanently block it).
//
// Runs as the non-root `ghostcam` user; the netdev polkit rule
// (49-ghostcam-nm.rules) grants the NetworkManager actions. Best-effort:
// logs and returns nil on nmcli errors so a modem-less camera isn't held
// up. Safe to run every boot (idempotent).
func EnsureCellular(ctx context.Context, apn, user, pass string) error {
	if apn == "" {
		return nil
	}
	if _, err := exec.LookPath("nmcli"); err != nil {
		slog.Warn("nmcli not available, cannot provision cellular", "err", err)
		return nil
	}

	out, err := exec.CommandContext(ctx, "nmcli", "-t", "-f", "NAME,TYPE", "connection", "show").Output()
	if err != nil {
		slog.Warn("cellular: nmcli connection show failed", "err", err)
		return nil
	}
	hasGSM, hasOurs := scanCellularConns(string(out), cellularConnName)

	switch {
	case hasOurs:
		slog.Info("cellular: syncing existing connection", "con", cellularConnName, "apn", apn)
		args := []string{"connection", "modify", cellularConnName,
			"gsm.apn", apn,
			"connection.autoconnect", "yes",
			"connection.autoconnect-retries", "0"}
		args = append(args, cellularCredArgs(user, pass)...)
		if o, e := exec.CommandContext(ctx, "nmcli", args...).CombinedOutput(); e != nil {
			slog.Warn("cellular: modify failed", "err", e, "output", strings.TrimSpace(string(o)))
			return nil
		}
	case hasGSM:
		slog.Info("cellular: a gsm connection already exists, leaving it as-is")
		return nil
	default:
		slog.Info("cellular: creating connection", "con", cellularConnName, "apn", apn)
		args := []string{"connection", "add", "type", "gsm",
			"con-name", cellularConnName,
			"ifname", "*",
			"gsm.apn", apn,
			"connection.autoconnect", "yes",
			"connection.autoconnect-retries", "0"}
		args = append(args, cellularCredArgs(user, pass)...)
		if o, e := exec.CommandContext(ctx, "nmcli", args...).CombinedOutput(); e != nil {
			slog.Warn("cellular: add failed", "err", e, "output", strings.TrimSpace(string(o)))
			return nil
		}
	}

	// Nudge it up now. NM's autoconnect would bring it up on its own, but
	// an explicit `up` shortens time-to-first-bearer on a cold boot. Non-
	// fatal — the modem may still be enumerating; autoconnect covers that.
	if o, e := exec.CommandContext(ctx, "nmcli", "connection", "up", cellularConnName).CombinedOutput(); e != nil {
		slog.Info("cellular: initial 'up' did not connect yet (autoconnect will retry)",
			"output", strings.TrimSpace(string(o)))
	} else {
		slog.Info("cellular: connection up", "con", cellularConnName)
	}
	return nil
}

// WaitForRoute blocks until a default route exists in /proc/net/route.
func WaitForRoute(ctx context.Context) {
	if DefaultInterface() != "" {
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
			if DefaultInterface() != "" {
				slog.Info("default route appeared", "elapsed_s", time.Since(start).Seconds())
				return
			}
		}
	}
}

// WaitForRouteTimeout waits up to timeout for a default route to appear.
// Returns true if a route was found, false if the timeout or context expired.
func WaitForRouteTimeout(ctx context.Context, timeout time.Duration) bool {
	if DefaultInterface() != "" {
		return true
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if DefaultInterface() != "" {
				return true
			}
		}
	}
}

// DefaultInterface reads the default route interface from /proc/net/route.
func DefaultInterface() string {
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
