// Package uplink implements the force-cellular dev/diagnostic control: a
// time-boxed, reboot-surviving switch that pushes the camera onto its
// cellular bearer by taking WiFi down, then restores WiFi automatically.
//
// Safety is the whole point. The "revert" is not an in-memory timer — it
// is a persisted expiry (force_cellular_until, unix ms) that a watchdog
// re-reads every few seconds and at every startup. So if the daemon
// restarts (or the box reboots) while WiFi is forced down, the watchdog
// re-arms if the deadline is still in the future and, crucially, restores
// WiFi the moment the deadline passes — even if that happened while the
// daemon was dead. A cellular link that never comes up therefore can't
// permanently strand a remote camera off-network.
package uplink

import (
	"context"
	"log/slog"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/network"
	"github.com/cargocam/ghostcam/camera/internal/state"
)

const (
	// ForceCellularMaxSeconds is the hard ceiling on a force window,
	// regardless of what the server requests — the outer bound on how long
	// a misfired command can hold WiFi down before the deadline restores it.
	ForceCellularMaxSeconds = 30 * 60

	forceUntilFile = "force_cellular_until"
	watchdogTick   = 3 * time.Second
)

// forceAction is the watchdog's decision for the current tick.
type forceAction int

const (
	actIdle forceAction = iota // no force set — don't touch WiFi
	actForce                   // deadline in the future — WiFi must be down
	actRestore                 // deadline passed (or revert requested) — WiFi back on
)

// decideForce is the pure enforcement decision, split out for tests.
// untilMs == 0 means "no force set". A non-zero deadline in the past means
// "expired / revert" (this is also how an explicit revert is encoded: the
// command writes a now-or-past deadline).
func decideForce(untilMs, nowMs int64) forceAction {
	switch {
	case untilMs > nowMs:
		return actForce
	case untilMs > 0:
		return actRestore
	default:
		return actIdle
	}
}

// ClampForceSeconds bounds a requested force duration to [0, max].
func ClampForceSeconds(s int) int {
	if s < 0 {
		return 0
	}
	if s > ForceCellularMaxSeconds {
		return ForceCellularMaxSeconds
	}
	return s
}

// SetForce persists the force-cellular deadline. seconds > 0 forces WiFi
// down for that many seconds (clamped); seconds <= 0 requests an immediate
// revert (encoded as a now-dated deadline so the watchdog's restore path
// runs and turns WiFi back on). The watchdog does the actual enforcement.
func SetForce(dataDir string, nowMs int64, seconds int) {
	var until int64
	if seconds > 0 {
		until = nowMs + int64(ClampForceSeconds(seconds))*1000
	} else {
		until = nowMs // already expired → restore on next tick
	}
	if err := state.WriteStoredFile(dataDir, forceUntilFile, strconv.FormatInt(until, 10)); err != nil {
		slog.Warn("force_cellular: persist deadline failed", "err", err)
	}
}

func readUntil(dataDir string) int64 {
	s := state.ReadTrimmedFile(filepath.Join(dataDir, forceUntilFile))
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// RunForceCellularWatchdog enforces the persisted force-cellular deadline
// until ctx is cancelled. Blocking; run as a goroutine. No-op in practice
// when no force is ever set (it only touches WiFi in response to a
// deadline). On synthetic / non-Linux builds network.SetWifiRadio is a
// stub so this is inert.
func RunForceCellularWatchdog(ctx context.Context, dataDir string) {
	applied := false // whether we've taken WiFi down for the current force
	t := time.NewTicker(watchdogTick)
	defer t.Stop()
	for {
		until := readUntil(dataDir)
		switch decideForce(until, time.Now().UnixMilli()) {
		case actForce:
			if !applied {
				if err := network.SetWifiRadio(ctx, false); err != nil {
					slog.Warn("force_cellular: could not disable WiFi, will retry", "err", err)
				} else {
					applied = true
					slog.Info("force_cellular: WiFi down, forced onto cellular", "until_ms", until)
				}
			}
		case actRestore:
			if err := network.SetWifiRadio(ctx, true); err != nil {
				slog.Warn("force_cellular: could not restore WiFi, will retry", "err", err)
			} else {
				// Clear the deadline (write 0) so we settle into idle and
				// don't keep re-running the restore path every tick.
				_ = state.WriteStoredFile(dataDir, forceUntilFile, "0")
				applied = false
				slog.Info("force_cellular: window ended, WiFi restored")
			}
		case actIdle:
			applied = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
