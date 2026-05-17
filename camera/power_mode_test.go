package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cargocam/ghostcam/common"
)

// resetPowerModeState restores package-level mutable state used by
// the power-mode tests so they don't contaminate each other or the
// ABR tests when go test runs everything in one process.
func resetPowerModeState(t *testing.T) {
	t.Helper()
	currentPowerMode.Store(nil)
	requestPipelineRestart.Store(false)
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

func TestHandleCommand_SetPowerMode_PersistsAndRequestsRestart(t *testing.T) {
	resetPowerModeState(t)
	t.Cleanup(func() { resetPowerModeState(t) })

	dir := t.TempDir()
	// Seed: live. Transitioning live -> sleep must persist the new
	// value to disk, swap the atomic, and trip
	// requestPipelineRestart so any active capture session tears down.
	SetPowerMode(PowerModeLive)

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_power_mode", PowerMode: PowerModeSleep},
		dir, nil)

	if got := CurrentPowerMode(); got != PowerModeSleep {
		t.Errorf("atomic not updated: got %q, want sleep", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "power_mode"))
	if err != nil {
		t.Fatalf("power_mode file not written: %v", err)
	}
	if string(data) != PowerModeSleep {
		t.Errorf("power_mode file contents = %q, want %q", string(data), PowerModeSleep)
	}
	if !requestPipelineRestart.Load() {
		t.Errorf("requestPipelineRestart not set on mode change")
	}
}

func TestHandleCommand_SetPowerMode_NoOpOnSameMode(t *testing.T) {
	// Same-mode set should still persist (so the file is the canonical
	// source) but should not trip requestPipelineRestart — there's
	// nothing to tear down.
	resetPowerModeState(t)
	t.Cleanup(func() { resetPowerModeState(t) })

	dir := t.TempDir()
	SetPowerMode(PowerModeLive)
	requestPipelineRestart.Store(false)

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_power_mode", PowerMode: PowerModeLive},
		dir, nil)

	if got := CurrentPowerMode(); got != PowerModeLive {
		t.Errorf("atomic changed unexpectedly: got %q, want live", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "power_mode")); err != nil {
		t.Errorf("power_mode file should still be written on same-mode set: %v", err)
	}
	if requestPipelineRestart.Load() {
		t.Errorf("requestPipelineRestart fired on no-op mode change")
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

func TestHandleCommand_SetPowerMode_RejectsInvalid(t *testing.T) {
	resetPowerModeState(t)
	t.Cleanup(func() { resetPowerModeState(t) })

	dir := t.TempDir()
	SetPowerMode(PowerModeLive)

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_power_mode", PowerMode: "bogus"},
		dir, nil)

	if got := CurrentPowerMode(); got != PowerModeLive {
		t.Errorf("invalid mode mutated state: now %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "power_mode")); !os.IsNotExist(err) {
		t.Errorf("invalid mode wrote power_mode file (should not)")
	}
	if requestPipelineRestart.Load() {
		t.Errorf("invalid mode tripped requestPipelineRestart")
	}
}
