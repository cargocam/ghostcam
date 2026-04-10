package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/cargocam/ghostcam/common"
)

// HandleCommand processes a server-issued command received via the telemetry
// poll response. Commands like reboot, unregister, and config changes cause
// the process to exit — systemd restarts it with new state.
func HandleCommand(ctx context.Context, cmd common.CameraCommand, dataDir string) {
	switch cmd.Type {
	case "reboot":
		slog.Info("reboot command received")
		os.Exit(0)
	case "unregister":
		slog.Info("unregister command received, clearing credentials")
		ClearCredentials(dataDir)
		os.Exit(0)
	case "set_recording_mode":
		slog.Info("recording mode change requested", "mode", cmd.Mode)
		if err := WriteStoredFile(dataDir, "recording_mode", cmd.Mode); err != nil {
			slog.Error("failed to persist recording_mode", "err", err)
			return
		}
		slog.Info("recording mode updated, restarting to apply")
		os.Exit(0)
	case "set_resolution":
		slog.Info("resolution change requested", "resolution", cmd.Resolution)
		if err := WriteStoredFile(dataDir, "resolution", cmd.Resolution); err != nil {
			slog.Error("failed to persist resolution", "err", err)
			return
		}
		slog.Info("resolution updated, restarting to apply")
		os.Exit(0)
	case "network_config":
		slog.Info("network config command", "ssid", cmd.SSID)
		go func() {
			psk := cmd.PSK
			if err := EnsureWifi(ctx, cmd.SSID, &psk); err != nil {
				slog.Warn("WiFi config failed", "err", err)
			}
		}()
	case "remove_network":
		slog.Info("remove network command", "ssid", cmd.SSID)
	default:
		slog.Warn("unknown command", "type", cmd.Type)
	}
}
