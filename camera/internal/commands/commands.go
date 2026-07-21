package commands

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/battery"
	"github.com/cargocam/ghostcam/camera/internal/diag"
	"github.com/cargocam/ghostcam/camera/internal/firmware"
	"github.com/cargocam/ghostcam/camera/internal/network"
	"github.com/cargocam/ghostcam/camera/internal/power"
	"github.com/cargocam/ghostcam/camera/internal/state"
	"github.com/cargocam/ghostcam/camera/internal/uplink"
	"github.com/cargocam/ghostcam/common"
)

// Client is the command dispatcher's narrow view of the server HTTP
// client. Currently only the firmware updater needs it; the surface is
// firmware.Client + a Version method so main can pass Version through
// without each subpackage reaching back into package main. The
// concrete *main.Client (camera/client.go) satisfies this surface.
type Client interface {
	firmware.Client
	Version() string
}

// HandleCommand processes a server-issued command received via the telemetry
// poll response. Commands like reboot, unregister, and config changes cause
// the process to exit — systemd restarts it with new state.
func HandleCommand(ctx context.Context, cmd common.CameraCommand, dataDir string, client Client) {
	switch cmd.Type {
	case "reboot":
		slog.Info("reboot command received")
		os.Exit(0)
	case "unregister":
		slog.Info("unregister command received, clearing credentials")
		state.ClearCredentials(dataDir)
		os.Exit(0)
	case "set_recording_mode":
		slog.Info("recording mode change requested", "mode", cmd.Mode)
		if err := state.WriteStoredFile(dataDir, "recording_mode", cmd.Mode); err != nil {
			slog.Error("failed to persist recording_mode", "err", err)
			return
		}
		slog.Info("recording mode updated, restarting to apply")
		os.Exit(0)
	case "set_resolution":
		slog.Info("resolution change requested", "resolution", cmd.Resolution)
		if err := state.WriteStoredFile(dataDir, "resolution", cmd.Resolution); err != nil {
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
		if !power.IsValidPowerMode(cmd.PowerMode) {
			slog.Error("invalid power mode, ignoring", "mode", cmd.PowerMode)
			return
		}
		if err := state.WriteStoredFile(dataDir, "power_mode", cmd.PowerMode); err != nil {
			slog.Error("failed to persist power_mode", "err", err)
			return
		}
		power.SetManualPowerMode(cmd.PowerMode)
		previous, effective := power.ApplyEffectivePowerMode(battery.LastBatteryPct())
		if previous != effective {
			// Tear down any in-flight capture session so the loop's
			// top-of-iteration check can re-evaluate the mode. Same
			// mechanism the telemetry-poll uses on WHIPSessionMissing
			// and ABR uses on tier shift.
			state.RequestPipelineRestart()
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
		var rules []battery.BatteryRule
		if cmd.BatteryRules != "" {
			if err := json.Unmarshal([]byte(cmd.BatteryRules), &rules); err != nil {
				slog.Error("set_battery_rules: invalid JSON", "err", err, "payload", cmd.BatteryRules)
				return
			}
		}
		if err := battery.SaveBatteryRules(dataDir, rules); err != nil {
			slog.Error("set_battery_rules: persist failed", "err", err)
			return
		}
		battery.SetBatteryRules(rules)
		previous, effective := power.ApplyEffectivePowerMode(battery.LastBatteryPct())
		if previous != effective {
			state.RequestPipelineRestart()
			slog.Info("battery rules updated, effective mode changed",
				"from", previous, "to", effective, "rule_count", len(rules))
		} else {
			slog.Info("battery rules updated", "count", len(rules))
		}
	case "network_config":
		slog.Info("network config command", "ssid", cmd.SSID)
		go func() {
			psk := cmd.PSK
			if err := network.EnsureWifi(ctx, cmd.SSID, &psk); err != nil {
				slog.Warn("WiFi config failed", "err", err)
			}
		}()
	case "set_cellular":
		// Deliver/refresh the cellular APN on a deployed camera without
		// SSH. Persist so it survives reboots (LoadConfig reads the file),
		// then apply immediately in the background (nmcli can block on a
		// cold modem). Empty APN is a no-op guard.
		slog.Info("set cellular command", "apn", cmd.CellularAPN)
		if cmd.CellularAPN == "" {
			break
		}
		state.PersistCellular(dataDir, cmd.CellularAPN, cmd.CellularUser, cmd.CellularPass)
		go func() {
			if err := network.EnsureCellular(ctx, cmd.CellularAPN, cmd.CellularUser, cmd.CellularPass); err != nil {
				slog.Warn("set_cellular apply failed", "err", err)
			}
		}()
	case "force_cellular":
		// Dev/diagnostic: force the camera onto cellular by taking WiFi
		// down for N seconds, then auto-restore. We only persist the
		// deadline here — the uplink watchdog (started in main) does the
		// enforcement and the reboot-surviving revert, so a failed cellular
		// link can't strand the camera. seconds<=0 reverts immediately.
		slog.Info("force cellular command", "seconds", cmd.ForceCellularSeconds)
		uplink.SetForce(dataDir, time.Now().UnixMilli(), cmd.ForceCellularSeconds)
	case "update_firmware":
		slog.Info("firmware update command received")
		if firmware.CheckFirmwareUpdate(ctx, client, dataDir, client.Version()) {
			os.Exit(0)
		}
	case "remove_network":
		slog.Info("remove network command", "ssid", cmd.SSID)
	case "diag_bundle":
		// Capture happens in a goroutine so HandleCommand returns
		// promptly and the telemetry-poll dispatcher can keep moving
		// through any other queued commands. The bundle lands in
		// pendingDiagBundles and rides out on the next poll.
		slog.Info("diag_bundle command received", "diag_id", cmd.DiagID)
		diagID := cmd.DiagID
		go func() {
			bundle := diag.CaptureDiagBundle(context.Background(), diagID)
			diag.AddPendingDiagBundle(bundle)
			slog.Info("diag_bundle captured, queued for next poll", "diag_id", diagID)
		}()
	case "restart_service":
		// os.Exit; systemd respawns us. No goroutine — we want to
		// take effect immediately.
		RestartGhostcamService()
	case "restart_modem_manager":
		go RestartModemManager(context.Background())
	case "restart_network_manager":
		go RestartNetworkManager(context.Background())
	default:
		slog.Warn("unknown command", "type", cmd.Type)
	}
}
