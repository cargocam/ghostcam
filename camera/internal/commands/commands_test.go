package commands

// Cross-package HandleCommand tests. These exercise the integration
// between commands.HandleCommand and power / battery state — both
// internal/power and internal/battery import this package indirectly
// (via commands → them), so the test files in those subpackages can't
// reference HandleCommand without creating a cycle. Tests landed here
// during the camera/ → internal/* refactor; subjects are unchanged.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cargocam/ghostcam/camera/internal/battery"
	"github.com/cargocam/ghostcam/camera/internal/power"
	"github.com/cargocam/ghostcam/camera/internal/state"
	"github.com/cargocam/ghostcam/common"
)

func uptr(v uint8) *uint8 { return &v }

// nullClient satisfies the Client interface for commands that don't
// actually need to talk to the server. set_battery_rules and
// set_power_mode dispatch paths never call into the client.
type nullClient struct{}

func (nullClient) HTTPClient() *http.Client { return nil }
func (nullClient) ServerURL() string        { return "" }
func (nullClient) Version() string          { return "dev" }

func TestApplyEffectivePowerMode_NoRulesEqualsManual(t *testing.T) {
	battery.SetBatteryRules(nil)
	power.SetManualPowerMode(power.PowerModeLive)
	power.SetPowerMode(power.PowerModeLive)

	_, effective := power.ApplyEffectivePowerMode(uptr(5))
	if effective != power.PowerModeLive {
		t.Fatalf("with no rules, effective should track manual=live, got %s", effective)
	}
}

func TestApplyEffectivePowerMode_RuleOverridesManual(t *testing.T) {
	battery.SetBatteryRules([]battery.BatteryRule{
		{Threshold: 15, PowerMode: power.PowerModeSleep},
	})
	power.SetManualPowerMode(power.PowerModeLive)
	power.SetPowerMode(power.PowerModeLive)

	previous, effective := power.ApplyEffectivePowerMode(uptr(10))
	if previous != power.PowerModeLive {
		t.Fatalf("previous: want live, got %s", previous)
	}
	if effective != power.PowerModeSleep {
		t.Fatalf("effective: want sleep (rule override), got %s", effective)
	}
	if power.ManualPowerMode() != power.PowerModeLive {
		t.Fatalf("manual must not be mutated by rule firing")
	}
}

func TestApplyEffectivePowerMode_RuleReleasesOnRecovery(t *testing.T) {
	// Discharge: rule fires, effective swaps to sleep.
	// Recharge: pct rises above threshold, effective reverts to
	// the user's manual mode. This is the "auto-revert on charge"
	// invariant.
	battery.SetBatteryRules([]battery.BatteryRule{
		{Threshold: 15, PowerMode: power.PowerModeSleep},
	})
	power.SetManualPowerMode(power.PowerModeLive)
	power.SetPowerMode(power.PowerModeLive)

	if _, eff := power.ApplyEffectivePowerMode(uptr(10)); eff != power.PowerModeSleep {
		t.Fatalf("discharge: want sleep, got %s", eff)
	}
	if _, eff := power.ApplyEffectivePowerMode(uptr(40)); eff != power.PowerModeLive {
		t.Fatalf("recharge: want live, got %s", eff)
	}
}

func TestApplyEffectivePowerMode_RejectsInvalidRulePowerMode(t *testing.T) {
	// A rule with a garbage power_mode shouldn't be able to put the
	// daemon in an undefined state — fall back to manual.
	battery.SetBatteryRules([]battery.BatteryRule{
		{Threshold: 50, PowerMode: "frobnicate"},
	})
	power.SetManualPowerMode(power.PowerModeLive)
	power.SetPowerMode(power.PowerModeLive)

	_, effective := power.ApplyEffectivePowerMode(uptr(40))
	if effective != power.PowerModeLive {
		t.Fatalf("invalid rule power_mode must not corrupt effective; got %s", effective)
	}
}

func TestHandleCommand_SetBatteryRules_PersistsAndApplies(t *testing.T) {
	dir := t.TempDir()
	power.SetManualPowerMode(power.PowerModeLive)
	power.SetPowerMode(power.PowerModeLive)
	// Cache a low battery reading so the rule will fire immediately.
	battery.RecordBatteryPct(uptr(5))
	state.ResetPipelineRestart()

	payload, _ := json.Marshal([]battery.BatteryRule{
		{Threshold: 10, PowerMode: power.PowerModeSleep, UploadMode: "lazy"},
	})

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_battery_rules", BatteryRules: string(payload)},
		dir, nullClient{})

	// Effective mode should have flipped to sleep, restart requested.
	if power.CurrentPowerMode() != power.PowerModeSleep {
		t.Fatalf("effective: want sleep, got %s", power.CurrentPowerMode())
	}
	if !state.IsPipelineRestartRequested() {
		t.Fatal("rule change should request pipeline restart when effective mode swaps")
	}
	if power.ManualPowerMode() != power.PowerModeLive {
		t.Fatalf("manual must remain live; rule overrides effective only, got %s", power.ManualPowerMode())
	}
	// Disk must hold the new rules so a restart re-loads them.
	loaded, err := battery.LoadBatteryRules(dir)
	if err != nil {
		t.Fatalf("load after handle: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Threshold != 10 {
		t.Fatalf("persisted rules wrong: %+v", loaded)
	}
}

func TestHandleCommand_SetBatteryRules_RejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	battery.SetBatteryRules([]battery.BatteryRule{{Threshold: 25, PowerMode: power.PowerModeStandby}})

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_battery_rules", BatteryRules: "{not-json"},
		dir, nullClient{})

	// Existing rules must remain in place — a malformed wire payload
	// should never silently clear an operator's setup.
	rules := battery.CurrentBatteryRules()
	if len(rules) != 1 || rules[0].Threshold != 25 {
		t.Fatalf("invalid JSON must not mutate active rules; got %+v", rules)
	}
	// And nothing should have been written to disk.
	if _, err := os.Stat(filepath.Join(dir, "battery_rules.json")); err == nil {
		t.Fatal("invalid JSON should not have touched disk")
	}
}

func TestHandleCommand_SetBatteryRules_EmptyClearsAll(t *testing.T) {
	dir := t.TempDir()
	battery.SetBatteryRules([]battery.BatteryRule{{Threshold: 25, PowerMode: power.PowerModeStandby}})

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_battery_rules", BatteryRules: ""},
		dir, nullClient{})

	if rules := battery.CurrentBatteryRules(); len(rules) != 0 {
		t.Fatalf("empty payload should clear rules; got %+v", rules)
	}
	loaded, err := battery.LoadBatteryRules(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("empty payload should persist as `[]`; got %+v", loaded)
	}
}

func TestHandleCommand_SetPowerMode_PersistsAndRequestsRestart(t *testing.T) {
	state.ResetPipelineRestart()
	power.SetPowerMode(power.PowerModeLive)
	battery.SetBatteryRules(nil)

	dir := t.TempDir()
	// Seed: live. Transitioning live -> sleep must persist the new
	// value to disk, swap the atomic, and trip
	// requestPipelineRestart so any active capture session tears down.
	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_power_mode", PowerMode: power.PowerModeSleep},
		dir, nullClient{})

	if got := power.CurrentPowerMode(); got != power.PowerModeSleep {
		t.Errorf("atomic not updated: got %q, want sleep", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "power_mode"))
	if err != nil {
		t.Fatalf("power_mode file not written: %v", err)
	}
	if string(data) != power.PowerModeSleep {
		t.Errorf("power_mode file contents = %q, want %q", string(data), power.PowerModeSleep)
	}
	if !state.IsPipelineRestartRequested() {
		t.Errorf("requestPipelineRestart not set on mode change")
	}
}

func TestHandleCommand_SetPowerMode_NoOpOnSameMode(t *testing.T) {
	// Same-mode set should still persist (so the file is the canonical
	// source) but should not trip requestPipelineRestart — there's
	// nothing to tear down.
	state.ResetPipelineRestart()
	battery.SetBatteryRules(nil)

	dir := t.TempDir()
	power.SetPowerMode(power.PowerModeLive)

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_power_mode", PowerMode: power.PowerModeLive},
		dir, nullClient{})

	if got := power.CurrentPowerMode(); got != power.PowerModeLive {
		t.Errorf("atomic changed unexpectedly: got %q, want live", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "power_mode")); err != nil {
		t.Errorf("power_mode file should still be written on same-mode set: %v", err)
	}
	if state.IsPipelineRestartRequested() {
		t.Errorf("requestPipelineRestart fired on no-op mode change")
	}
}

func TestHandleCommand_SetPowerMode_RejectsInvalid(t *testing.T) {
	state.ResetPipelineRestart()
	battery.SetBatteryRules(nil)

	dir := t.TempDir()
	power.SetPowerMode(power.PowerModeLive)

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_power_mode", PowerMode: "bogus"},
		dir, nullClient{})

	if got := power.CurrentPowerMode(); got != power.PowerModeLive {
		t.Errorf("invalid mode mutated state: now %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "power_mode")); !os.IsNotExist(err) {
		t.Errorf("invalid mode wrote power_mode file (should not)")
	}
	if state.IsPipelineRestartRequested() {
		t.Errorf("invalid mode tripped requestPipelineRestart")
	}
}
