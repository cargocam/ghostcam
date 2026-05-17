package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cargocam/ghostcam/common"
)

func uptr(v uint8) *uint8 { return &v }

func TestEvaluateBatteryRules(t *testing.T) {
	rules := []BatteryRule{
		{Threshold: 20, PowerMode: PowerModeStandby},
		{Threshold: 10, PowerMode: PowerModeSleep},
	}
	SetBatteryRules(rules)
	sorted := CurrentBatteryRules()

	cases := []struct {
		name string
		pct  *uint8
		want string // "" = no rule
	}{
		{"nil pct, no rule fires", nil, ""},
		{"100% above all thresholds", uptr(100), ""},
		{"21% above all thresholds", uptr(21), ""},
		{"exactly at standby threshold", uptr(20), PowerModeStandby},
		{"between thresholds picks standby", uptr(15), PowerModeStandby},
		{"exactly at sleep threshold picks sleep", uptr(10), PowerModeSleep},
		{"below sleep threshold picks sleep", uptr(5), PowerModeSleep},
		{"0% picks sleep", uptr(0), PowerModeSleep},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EvaluateBatteryRules(sorted, c.pct)
			switch {
			case c.want == "" && got != nil:
				t.Fatalf("expected no rule, got %+v", got)
			case c.want != "" && got == nil:
				t.Fatalf("expected rule with PowerMode=%s, got nil", c.want)
			case c.want != "" && got.PowerMode != c.want:
				t.Fatalf("expected PowerMode=%s, got %s", c.want, got.PowerMode)
			}
		})
	}
}

func TestEvaluateBatteryRules_OutOfOrderInputStillResolves(t *testing.T) {
	// SetBatteryRules sorts highest-threshold first regardless of the
	// order callers supply, so the editor UI can hand us rules in any
	// order without affecting which rule wins.
	SetBatteryRules([]BatteryRule{
		{Threshold: 10, PowerMode: PowerModeSleep},
		{Threshold: 50, PowerMode: PowerModeStandby},
		{Threshold: 30, PowerMode: PowerModeLive}, // intermediate
	})
	rules := CurrentBatteryRules()
	if rules[0].Threshold != 10 || rules[2].Threshold != 50 {
		t.Fatalf("rules not sorted lowest-first: %+v", rules)
	}
	// At 40 %, only the 50 % rule matches (10 % and 30 % don't), so we
	// pick that one.
	got := EvaluateBatteryRules(rules, uptr(40))
	if got == nil || got.Threshold != 50 {
		t.Fatalf("expected the 50%% rule at 40%%, got %+v", got)
	}
	// At 8 %, all three rules match — most-aggressive wins (lowest
	// threshold = 10).
	got = EvaluateBatteryRules(rules, uptr(8))
	if got == nil || got.Threshold != 10 {
		t.Fatalf("expected the 10%% rule at 8%%, got %+v", got)
	}
}

func TestApplyEffectivePowerMode_NoRulesEqualsManual(t *testing.T) {
	SetBatteryRules(nil)
	SetManualPowerMode(PowerModeLive)
	SetPowerMode(PowerModeLive)

	_, effective := ApplyEffectivePowerMode(uptr(5))
	if effective != PowerModeLive {
		t.Fatalf("with no rules, effective should track manual=live, got %s", effective)
	}
}

func TestApplyEffectivePowerMode_RuleOverridesManual(t *testing.T) {
	SetBatteryRules([]BatteryRule{
		{Threshold: 15, PowerMode: PowerModeSleep},
	})
	SetManualPowerMode(PowerModeLive)
	SetPowerMode(PowerModeLive)

	previous, effective := ApplyEffectivePowerMode(uptr(10))
	if previous != PowerModeLive {
		t.Fatalf("previous: want live, got %s", previous)
	}
	if effective != PowerModeSleep {
		t.Fatalf("effective: want sleep (rule override), got %s", effective)
	}
	if ManualPowerMode() != PowerModeLive {
		t.Fatalf("manual must not be mutated by rule firing")
	}
}

func TestApplyEffectivePowerMode_RuleReleasesOnRecovery(t *testing.T) {
	// Discharge: rule fires, effective swaps to sleep.
	// Recharge: pct rises above threshold, effective reverts to
	// the user's manual mode. This is the "auto-revert on charge"
	// invariant.
	SetBatteryRules([]BatteryRule{
		{Threshold: 15, PowerMode: PowerModeSleep},
	})
	SetManualPowerMode(PowerModeLive)
	SetPowerMode(PowerModeLive)

	if _, eff := ApplyEffectivePowerMode(uptr(10)); eff != PowerModeSleep {
		t.Fatalf("discharge: want sleep, got %s", eff)
	}
	if _, eff := ApplyEffectivePowerMode(uptr(40)); eff != PowerModeLive {
		t.Fatalf("recharge: want live, got %s", eff)
	}
}

func TestApplyEffectivePowerMode_RejectsInvalidRulePowerMode(t *testing.T) {
	// A rule with a garbage power_mode shouldn't be able to put the
	// daemon in an undefined state — fall back to manual.
	SetBatteryRules([]BatteryRule{
		{Threshold: 50, PowerMode: "frobnicate"},
	})
	SetManualPowerMode(PowerModeLive)
	SetPowerMode(PowerModeLive)

	_, effective := ApplyEffectivePowerMode(uptr(40))
	if effective != PowerModeLive {
		t.Fatalf("invalid rule power_mode must not corrupt effective; got %s", effective)
	}
}

func TestLoadSaveBatteryRules_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := []BatteryRule{
		{Threshold: 20, PowerMode: PowerModeStandby, UploadMode: "lazy"},
		{Threshold: 10, PowerMode: PowerModeSleep, UploadMode: "lazy"},
	}
	if err := SaveBatteryRules(dir, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadBatteryRules(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len mismatch: want %d got %d (%+v)", len(in), len(out), out)
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("rule[%d] mismatch: want %+v got %+v", i, in[i], out[i])
		}
	}
}

func TestLoadBatteryRules_MissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	rules, err := LoadBatteryRules(dir)
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("missing file should return no rules, got %+v", rules)
	}
}

func TestLoadBatteryRules_CorruptFileSurfacesError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, batteryRulesFilename), []byte("not json"), 0644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	_, err := LoadBatteryRules(dir)
	if err == nil {
		t.Fatal("corrupt file should surface a parse error so the operator notices")
	}
}

func TestSaveBatteryRules_EmptyWritesExplicitArray(t *testing.T) {
	// Operator-cleared rules vs never-had-rules should be
	// distinguishable on disk (so a future reader can show "explicitly
	// cleared" in the UI).
	dir := t.TempDir()
	if err := SaveBatteryRules(dir, nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, batteryRulesFilename))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("expected `[]`, got %q", string(data))
	}
}

func TestHandleCommand_SetBatteryRules_PersistsAndApplies(t *testing.T) {
	dir := t.TempDir()
	SetManualPowerMode(PowerModeLive)
	SetPowerMode(PowerModeLive)
	// Cache a low battery reading so the rule will fire immediately.
	RecordBatteryPct(uptr(5))
	requestPipelineRestart.Store(false)

	payload, _ := json.Marshal([]BatteryRule{
		{Threshold: 10, PowerMode: PowerModeSleep, UploadMode: "lazy"},
	})

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_battery_rules", BatteryRules: string(payload)},
		dir, nil)

	// Effective mode should have flipped to sleep, restart requested.
	if CurrentPowerMode() != PowerModeSleep {
		t.Fatalf("effective: want sleep, got %s", CurrentPowerMode())
	}
	if !requestPipelineRestart.Load() {
		t.Fatal("rule change should request pipeline restart when effective mode swaps")
	}
	if ManualPowerMode() != PowerModeLive {
		t.Fatalf("manual must remain live; rule overrides effective only, got %s", ManualPowerMode())
	}
	// Disk must hold the new rules so a restart re-loads them.
	loaded, err := LoadBatteryRules(dir)
	if err != nil {
		t.Fatalf("load after handle: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Threshold != 10 {
		t.Fatalf("persisted rules wrong: %+v", loaded)
	}
}

func TestHandleCommand_SetBatteryRules_RejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	SetBatteryRules([]BatteryRule{{Threshold: 25, PowerMode: PowerModeStandby}})

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_battery_rules", BatteryRules: "{not-json"},
		dir, nil)

	// Existing rules must remain in place — a malformed wire payload
	// should never silently clear an operator's setup.
	rules := CurrentBatteryRules()
	if len(rules) != 1 || rules[0].Threshold != 25 {
		t.Fatalf("invalid JSON must not mutate active rules; got %+v", rules)
	}
	// And nothing should have been written to disk.
	if _, err := os.Stat(filepath.Join(dir, batteryRulesFilename)); err == nil {
		t.Fatal("invalid JSON should not have touched disk")
	}
}

func TestHandleCommand_SetBatteryRules_EmptyClearsAll(t *testing.T) {
	dir := t.TempDir()
	SetBatteryRules([]BatteryRule{{Threshold: 25, PowerMode: PowerModeStandby}})

	HandleCommand(context.Background(),
		common.CameraCommand{Type: "set_battery_rules", BatteryRules: ""},
		dir, nil)

	if rules := CurrentBatteryRules(); len(rules) != 0 {
		t.Fatalf("empty payload should clear rules; got %+v", rules)
	}
	loaded, err := LoadBatteryRules(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("empty payload should persist as `[]`; got %+v", loaded)
	}
}
