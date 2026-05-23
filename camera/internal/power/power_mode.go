package power

import (
	"sync/atomic"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/battery"
	"github.com/cargocam/ghostcam/camera/internal/state"
)

// Power modes (#112). The server enqueues `set_power_mode` commands with
// one of three values:
//
//   live    — always-on capture, telemetry every 10 s, WHIP publisher
//             stays connected. The current default behaviour.
//   standby — recording continues but the WHIP publisher is opened on
//             demand only (server's wake_live flag drives it).
//   sleep   — capture suspended (no rpicam-vid), telemetry every 5 min,
//             enough only to receive the wake command.
//
// Two layers (#73 added the manual/effective split):
//
//   manualPowerMode — what the operator set via set_power_mode (or
//                     seeded from disk on startup). Persists across
//                     reboots in {dataDir}/power_mode.
//   currentPowerMode — the effective mode the capture loop reads. Equal
//                     to manualPowerMode unless a battery rule is
//                     currently firing, in which case it's the rule's
//                     PowerMode.
//
// ApplyEffectivePowerMode resolves the two atomics + the current
// battery rule set + the cached battery_pct into a final effective mode
// and writes it to currentPowerMode. Called from set_power_mode (after
// manual change) and from the telemetry-poll loop (after each
// battery_pct sample).

// Power-mode string constants and IsValidPowerMode live in
// internal/state so internal/battery can build default rules that
// reference them without importing power (which would cycle). The
// re-exports here keep the daemon's spelling — power.PowerModeLive,
// power.IsValidPowerMode — unchanged at call sites that already
// import power.
const (
	PowerModeLive    = state.PowerModeLive
	PowerModeStandby = state.PowerModeStandby
	PowerModeSleep   = state.PowerModeSleep
)

// IsValidPowerMode is the same validation set the server applies in
// cameras.go before enqueueing the command. Delegates to state so the
// canonical definition lives in one place.
func IsValidPowerMode(mode string) bool { return state.IsValidPowerMode(mode) }

// currentPowerMode is the effective mode the capture loop reads.
var currentPowerMode atomic.Pointer[string]

// manualPowerMode is the operator's last explicit setting. Battery
// rules layer on top of this — they don't overwrite it.
var manualPowerMode atomic.Pointer[string]

// SetPowerMode atomically swaps the effective mode. Used by
// ApplyEffectivePowerMode; callers outside this file should generally
// go through ApplyEffectivePowerMode instead of writing the effective
// mode directly.
func SetPowerMode(mode string) {
	m := mode
	currentPowerMode.Store(&m)
}

// CurrentPowerMode returns the live effective mode, defaulting to
// "live" when the atomic hasn't been seeded yet (test contexts).
func CurrentPowerMode() string {
	if m := currentPowerMode.Load(); m != nil {
		return *m
	}
	return PowerModeLive
}

// SetManualPowerMode records the operator's explicit choice. Does NOT
// re-resolve the effective mode — call ApplyEffectivePowerMode after
// for that.
func SetManualPowerMode(mode string) {
	m := mode
	manualPowerMode.Store(&m)
}

// ManualPowerMode returns the operator's explicit setting, defaulting
// to "live" when the atomic hasn't been seeded yet.
func ManualPowerMode() string {
	if m := manualPowerMode.Load(); m != nil {
		return *m
	}
	return PowerModeLive
}

// ApplyEffectivePowerMode recomputes the effective mode from the manual
// setting plus any active battery rule for the given pct, writes it to
// currentPowerMode, and returns (previous, effective) for the caller's
// own logging / restart decisions. pct may be nil — rule evaluation
// short-circuits and the effective mode equals the manual mode.
func ApplyEffectivePowerMode(pct *uint8) (previous, effective string) {
	manual := ManualPowerMode()
	effective = manual
	if rule := battery.EvaluateBatteryRules(battery.CurrentBatteryRules(), pct); rule != nil {
		if IsValidPowerMode(rule.PowerMode) {
			effective = rule.PowerMode
		}
	}
	previous = CurrentPowerMode()
	SetPowerMode(effective)
	return previous, effective
}

// standbyWakeUntilUnix carries the unix-second timestamp until which
// the camera should treat a viewer as actively watching. Set when
// telemetry-poll sees `WakeLive=true` in the server's response;
// refreshed by every subsequent WakeLive=true. While now < this value
// the capture loop (in Standby mode) spawns a WHIP publisher; when
// the window expires the watchdog tears down the publisher again so
// Standby actually saves the cellular bandwidth it promises.
var standbyWakeUntilUnix atomic.Int64

// standbyWakeWindow is how long a single WakeLive=true keeps the
// publisher open before the watchdog tears it down. Sized to the
// server-side wake_live key TTL so a refresh from any subsequent
// viewer-side WHEP attempt keeps the publisher alive across normal
// browsing churn.
const standbyWakeWindow = 5 * time.Minute

// MarkStandbyWake records a viewer-attached signal so the capture
// loop can keep / spawn a WHIP publisher for the next
// standbyWakeWindow. Idempotent — repeat calls just extend the window.
func MarkStandbyWake() {
	standbyWakeUntilUnix.Store(time.Now().Add(standbyWakeWindow).Unix())
}

// StandbyWakeActive reports whether the most recent WakeLive signal
// is still within its window. Returns true in Live mode regardless
// (Live always wants the publisher); the capture loop owns the
// per-mode decision.
func StandbyWakeActive() bool {
	return time.Now().Unix() < standbyWakeUntilUnix.Load()
}
