package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"

	"github.com/cargocam/ghostcam/common"
)

// HandleCommand processes a server-issued command received via the telemetry
// poll response. Commands like reboot, unregister, and config changes cause
// the process to exit — systemd restarts it with new state.
func HandleCommand(ctx context.Context, cmd common.CameraCommand, dataDir string, client *Client) {
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
	case "set_power_mode":
		// Power-mode changes are handled in-process (no os.Exit) because
		// the sleep mode needs telemetry to keep flowing so the wake
		// command can arrive — restarting the daemon every time would
		// break that loop.
		slog.Info("power mode change requested", "mode", cmd.PowerMode)
		if !IsValidPowerMode(cmd.PowerMode) {
			slog.Error("invalid power mode, ignoring", "mode", cmd.PowerMode)
			return
		}
		if err := WriteStoredFile(dataDir, "power_mode", cmd.PowerMode); err != nil {
			slog.Error("failed to persist power_mode", "err", err)
			return
		}
		SetManualPowerMode(cmd.PowerMode)
		previous, effective := ApplyEffectivePowerMode(LastBatteryPct())
		if previous != effective {
			// Tear down any in-flight capture session so the loop's
			// top-of-iteration check can re-evaluate the mode. Same
			// mechanism the telemetry-poll uses on WHIPSessionMissing
			// and ABR uses on tier shift.
			requestPipelineRestart.Store(true)
			slog.Info("power mode updated",
				"from", previous, "to", effective, "manual", cmd.PowerMode)
		}
	case "set_battery_rules":
		// Battery rules are level-triggered against the most recent
		// battery_pct sample. Persisting them and swapping the in-process
		// atomic is enough — no pipeline restart, because the rules only
		// take effect on the next telemetry tick, where the standard
		// ApplyEffectivePowerMode path will fire requestPipelineRestart
		// if the effective mode changed.
		var rules []BatteryRule
		if cmd.BatteryRules != "" {
			if err := json.Unmarshal([]byte(cmd.BatteryRules), &rules); err != nil {
				slog.Error("set_battery_rules: invalid JSON", "err", err, "payload", cmd.BatteryRules)
				return
			}
		}
		if err := SaveBatteryRules(dataDir, rules); err != nil {
			slog.Error("set_battery_rules: persist failed", "err", err)
			return
		}
		SetBatteryRules(rules)
		previous, effective := ApplyEffectivePowerMode(LastBatteryPct())
		if previous != effective {
			requestPipelineRestart.Store(true)
			slog.Info("battery rules updated, effective mode changed",
				"from", previous, "to", effective, "rule_count", len(rules))
		} else {
			slog.Info("battery rules updated", "count", len(rules))
		}
	case "network_config":
		slog.Info("network config command", "ssid", cmd.SSID)
		go func() {
			psk := cmd.PSK
			if err := EnsureWifi(ctx, cmd.SSID, &psk); err != nil {
				slog.Warn("WiFi config failed", "err", err)
			}
		}()
	case "update_firmware":
		slog.Info("firmware update command received")
		if CheckFirmwareUpdate(ctx, client, dataDir) {
			os.Exit(0)
		}
	case "remove_network":
		slog.Info("remove network command", "ssid", cmd.SSID)
	default:
		slog.Warn("unknown command", "type", cmd.Type)
	}
}
