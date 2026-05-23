package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/battery"
	"github.com/cargocam/ghostcam/camera/internal/commands"
	"github.com/cargocam/ghostcam/camera/internal/power"
	"github.com/cargocam/ghostcam/camera/internal/sensors"
	"github.com/cargocam/ghostcam/camera/internal/state"
	"github.com/cargocam/ghostcam/common"
)

// Client is the telemetry-poll loop's narrow view of the server HTTP
// client. The concrete *main.Client (camera/client.go) satisfies this
// surface; defining the interface here lets telemetry live in its own
// subpackage without importing package main.
type Client interface {
	PostTelemetry(ctx context.Context, telemetry common.TelemetryDatagram, rollbackEventJSON string) (common.TelemetryPollResponse, error)
}

// CommandClient is the additional surface HandleCommand needs from the
// HTTP client (firmware updates carry context the dispatcher uses
// without re-wiring). It must be a superset of Client; main's Client
// satisfies both.
type CommandClient interface {
	commands.Client
}

// RunTelemetryPoll sends telemetry to the server every 10s, processes
// piggy-backed commands, and backs off on consecutive failures.
// Sleep power mode (#112) overrides the cadence: every 5 min while in
// sleep, so the daemon can still receive the wake command while
// spending most of its time idle. Backoff during sleep is bypassed —
// 5 min already exceeds maxInterval.
func RunTelemetryPoll(ctx context.Context, client interface {
	Client
	commands.Client
}, dataDir string) {
	const (
		baseInterval  = 10 * time.Second
		maxInterval   = 60 * time.Second
		sleepInterval = 5 * time.Minute
	)
	interval := baseInterval
	consecutiveFailures := 0
	healthMarked := false

	for {
		// Sleep mode overrides any backoff state — same way set_power_mode
		// is the operator's explicit decision.
		if power.CurrentPowerMode() == power.PowerModeSleep {
			interval = sleepInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			telemetry := sensors.ReadTelemetry()

			// Battery rule evaluation (#73). Cache the sampled pct so
			// HandleCommand can re-resolve rules without re-querying
			// the hardware, then re-apply the effective power mode. A
			// level-triggered rule that just started (or just stopped)
			// firing will swap the effective mode and trip the capture
			// supervisor's restart flag.
			battery.RecordBatteryPct(telemetry.BatteryPct)
			previous, effective := power.ApplyEffectivePowerMode(telemetry.BatteryPct)
			if previous != effective {
				state.RequestPipelineRestart()
				slog.Info("battery rule changed effective power mode",
					"from", previous, "to", effective,
					"battery_pct_present", telemetry.BatteryPct != nil)
			}
			// Surface the effective mode in the telemetry envelope so
			// the server / UI can show what the camera is actually
			// doing (manual mode vs rule override).
			eff := effective
			telemetry.PowerMode = &eff

			// Drain the rollback marker, if any. ExecStartPre writes
			// the rollback reason as the file's contents:
			//   "no-boot-ok"    — daemon never reached telemetry
			//   "brief-uptime"  — daemon reached server but crashed
			//                     before firmwareStabilityMinutes (#106)
			// Pre-#106 installs wrote a zero-byte sentinel; treat
			// empty content as the legacy "missing boot_ok" reason for
			// back-compat with markers still on disk from earlier
			// versions.
			rollbackPath := filepath.Join(dataDir, "rollback_pending")
			rollbackJSON := ""
			if data, err := os.ReadFile(rollbackPath); err == nil {
				reason := strings.TrimSpace(string(data))
				if reason == "" {
					reason = "missing boot_ok"
				}
				payload, _ := json.Marshal(map[string]string{
					"at":     time.Now().UTC().Format(time.RFC3339),
					"reason": reason,
				})
				rollbackJSON = string(payload)
			}

			resp, err := client.PostTelemetry(ctx, telemetry, rollbackJSON)
			if err != nil {
				consecutiveFailures++
				slog.Debug("telemetry POST failed", "err", err, "consecutive_failures", consecutiveFailures)
				switch {
				case consecutiveFailures >= 3:
					interval = maxInterval
				case consecutiveFailures >= 2:
					interval = 30 * time.Second
				default:
					interval = baseInterval
				}
				continue
			}
			if consecutiveFailures > 0 {
				consecutiveFailures = 0
				interval = baseInterval
			}

			// Server accepted the rollback marker — safe to consume.
			if rollbackJSON != "" {
				_ = os.Remove(rollbackPath)
				slog.Info("rollback event surfaced to server", "payload", rollbackJSON)
			}

			// Write boot_ok marker after first successful telemetry.
			// ExecStartPre checks this to decide whether to roll back
			// a staged firmware update on the next restart.
			if !healthMarked {
				_ = os.WriteFile(filepath.Join(dataDir, "boot_ok"), nil, 0644)
				healthMarked = true
			}

			// Server lost the WHIP session (likely a redeploy/crash).
			// Two paths back to LIVE depending on local state:
			//   * pub != nil — close it; pc.Disconnected fires, capture
			//     pipeline tears down, outer loop reconnects.
			//   * pub == nil — capture is already running without live
			//     because the initial WHIP connect failed (e.g. POST
			//     timed out mid-deploy). The outer loop only re-runs
			//     when capture exits, so we set a flag the capture
			//     supervisor watches and cancels the pipeline from.
			if resp.WHIPSessionMissing {
				if pub := state.CurrentPublisher(); pub != nil {
					slog.Warn("server reports WHIP session missing; closing local publisher to force reconnect")
					_ = pub.Close()
				} else {
					slog.Warn("server reports WHIP session missing and local publisher is nil; requesting pipeline restart")
					state.RequestPipelineRestart()
				}
			}

			// Standby wake (Standby mode). Server-side WHEPOffer parks
			// a wake_live flag in Redis when a viewer tries to attach
			// to a sleeping camera; PostTelemetry forwards it here.
			// Refresh the wake window every time we see it — sustained
			// viewing wins by virtue of any subsequent WHEP retry
			// re-setting the flag server-side. In Live mode the flag
			// is harmless: the publisher is always open already.
			if resp.WakeLive {
				wasActive := power.StandbyWakeActive()
				power.MarkStandbyWake()
				if !wasActive && power.CurrentPowerMode() == power.PowerModeStandby {
					// Transition idle → wake while in Standby. Trip
					// the capture pipeline so the next spawn includes
					// a publisher.
					slog.Info("standby wake received; opening publisher on next cycle")
					state.RequestPipelineRestart()
				}
			}

			for _, cmd := range resp.Commands {
				commands.HandleCommand(ctx, cmd, dataDir, client)
			}
		}
	}
}
