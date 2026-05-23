package power

import (
	"testing"
)

// resetPowerModeState restores package-level mutable state used by
// the power-mode tests so they don't contaminate each other or the
// ABR tests when go test runs everything in one process.
func resetPowerModeState(t *testing.T) {
	t.Helper()
	currentPowerMode.Store(nil)
}

func TestSetPowerMode_CurrentPowerMode_RoundTrip(t *testing.T) {
	resetPowerModeState(t)
	t.Cleanup(func() { resetPowerModeState(t) })

	// Default (unseeded) state reads back as "live" so the capture
	// loop doesn't accidentally enter sleep on a fresh install.
	if got := CurrentPowerMode(); got != PowerModeLive {
		t.Errorf("unseeded CurrentPowerMode() = %q, want live", got)
	}

	for _, mode := range []string{PowerModeLive, PowerModeStandby, PowerModeSleep} {
		SetPowerMode(mode)
		if got := CurrentPowerMode(); got != mode {
			t.Errorf("after SetPowerMode(%q), CurrentPowerMode() = %q", mode, got)
		}
	}
}

func TestIsValidPowerMode(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{PowerModeLive, true},
		{PowerModeStandby, true},
		{PowerModeSleep, true},
		{"", false},
		{"asleep", false},
		{"LIVE", false},     // case-sensitive: server normalises before sending
		{"deep-sleep", false},
	}
	for _, c := range cases {
		if got := IsValidPowerMode(c.in); got != c.want {
			t.Errorf("IsValidPowerMode(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestStandbyWake_RoundTrip(t *testing.T) {
	// Default state: no wake → not active.
	standbyWakeUntilUnix.Store(0)
	if StandbyWakeActive() {
		t.Errorf("unset wake should report inactive")
	}

	// MarkStandbyWake sets a future timestamp → active until window
	// elapses. Test only the active branch; the elapsed branch is
	// covered below by directly setting the atomic to a past value.
	MarkStandbyWake()
	if !StandbyWakeActive() {
		t.Errorf("just-marked wake should be active")
	}

	// Window in the past → inactive (mid-session viewer-left case).
	standbyWakeUntilUnix.Store(1) // unix-1970, way in the past
	if StandbyWakeActive() {
		t.Errorf("past wake-until should report inactive")
	}
}

// The TestHandleCommand_SetPowerMode_* tests live in
// internal/commands/commands_test.go because they exercise the
// cross-package interaction between commands.HandleCommand and
// power.SetManualPowerMode / power.ApplyEffectivePowerMode. Keeping
// them here would create a power → commands import cycle (commands
// already imports power). See refactor/camera-subpackages PR notes.
