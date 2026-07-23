// Package uplink implements the force-cellular dev/diagnostic control: a
// time-boxed switch that pushes the camera onto its cellular bearer by
// taking WiFi down, then restores WiFi automatically.
//
// SAFETY / FAIL-OPEN. An earlier version persisted the force deadline to
// disk so it survived a reboot — which nearly stranded a field unit: a
// reboot re-applied the persisted "WiFi off" and, with cellular not
// carrying traffic, the camera went dark with no way back. The rule now is
// the opposite and absolute: **a process restart or reboot must always
// bring WiFi back**. So the force is in-memory only (lost on restart), and
// the watchdog's very first action on startup is to re-enable WiFi and
// delete any stale on-disk marker from the old design. A misfired or
// forgotten force can survive at most one daemon lifetime, and any restart
// recovers the network. The trade-off — a daemon crash mid-force ends the
// force early — is the safe direction.
package uplink

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/network"
)

const (
	// ForceCellularMaxSeconds is the hard ceiling on a force window,
	// regardless of what the server requests.
	ForceCellularMaxSeconds = 30 * 60

	// legacyForceFile is the old persisted-deadline marker. We no longer
	// write it; the watchdog deletes it on startup so a unit upgrading from
	// the persisted design can't inherit a stale WiFi-off.
	legacyForceFile = "force_cellular_until"
	watchdogTick    = 3 * time.Second
)

// forceUntil is the in-memory force deadline (unix ms), 0 = no force.
// Set by SetForce (telemetry command handler), read by the watchdog.
var forceUntil atomic.Int64

// forceAction is the watchdog's decision for the current tick.
type forceAction int

const (
	actIdle    forceAction = iota // no force set — don't touch WiFi
	actForce                      // deadline in the future — WiFi must be down
	actRestore                    // deadline passed / revert — WiFi back on
)

// decideForce is the pure enforcement decision, split out for tests.
// untilMs == 0 means "no force set". A non-zero deadline at/behind now
// means "expired / revert".
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

// SetForce sets the in-memory force-cellular deadline. seconds > 0 forces
// WiFi down for that many seconds (clamped); seconds <= 0 requests an
// immediate revert (a now-dated deadline so the watchdog's restore path
// runs). In-memory only — never persisted, so a restart always clears it.
func SetForce(nowMs int64, seconds int) {
	if seconds > 0 {
		forceUntil.Store(nowMs + int64(ClampForceSeconds(seconds))*1000)
	} else {
		forceUntil.Store(nowMs) // already expired → restore on next tick
	}
}

// RunForceCellularWatchdog enforces the in-memory force deadline until ctx
// is cancelled. Blocking; run as a goroutine.
func RunForceCellularWatchdog(ctx context.Context, dataDir string) {
	// FAIL-OPEN on startup: unconditionally bring WiFi back and drop any
	// stale legacy marker, so a restart/reboot can NEVER leave the camera
	// stranded with WiFi forced off. A fresh process has no in-memory force,
	// so this only ever un-does a leftover force-off (harmless no-op
	// otherwise — `nmcli radio wifi on` when already on).
	_ = os.Remove(filepath.Join(dataDir, legacyForceFile))
	if err := network.SetWifiRadio(ctx, true); err != nil {
		slog.Debug("force_cellular: startup WiFi-restore failed (likely already on)", "err", err)
	}

	applied := false // whether we've taken WiFi down for the current force
	t := time.NewTicker(watchdogTick)
	defer t.Stop()
	for {
		switch decideForce(forceUntil.Load(), time.Now().UnixMilli()) {
		case actForce:
			if !applied {
				if err := network.SetWifiRadio(ctx, false); err != nil {
					slog.Warn("force_cellular: could not disable WiFi, will retry", "err", err)
				} else {
					applied = true
					slog.Info("force_cellular: WiFi down, forced onto cellular")
				}
			}
		case actRestore:
			if err := network.SetWifiRadio(ctx, true); err != nil {
				slog.Warn("force_cellular: could not restore WiFi, will retry", "err", err)
			} else {
				forceUntil.Store(0)
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
