package battery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cargocam/ghostcam/camera/internal/state"
)

func uptr(v uint8) *uint8 { return &v }

func TestEvaluateBatteryRules(t *testing.T) {
	rules := []BatteryRule{
		{Threshold: 20, PowerMode: state.PowerModeStandby},
		{Threshold: 10, PowerMode: state.PowerModeSleep},
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
		{"exactly at standby threshold", uptr(20), state.PowerModeStandby},
		{"between thresholds picks standby", uptr(15), state.PowerModeStandby},
		{"exactly at sleep threshold picks sleep", uptr(10), state.PowerModeSleep},
		{"below sleep threshold picks sleep", uptr(5), state.PowerModeSleep},
		{"0% picks sleep", uptr(0), state.PowerModeSleep},
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
		{Threshold: 10, PowerMode: state.PowerModeSleep},
		{Threshold: 50, PowerMode: state.PowerModeStandby},
		{Threshold: 30, PowerMode: state.PowerModeLive}, // intermediate
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

func TestLoadSaveBatteryRules_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := []BatteryRule{
		{Threshold: 20, PowerMode: state.PowerModeStandby, UploadMode: "lazy"},
		{Threshold: 10, PowerMode: state.PowerModeSleep, UploadMode: "lazy"},
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

// The TestApplyEffectivePowerMode_* and TestHandleCommand_SetBatteryRules_*
// tests live in internal/commands/commands_test.go because they exercise
// the cross-package interaction between commands.HandleCommand,
// power.ApplyEffectivePowerMode, and battery rule evaluation. Keeping
// them here would create a battery → commands import cycle (commands
// already imports battery). See refactor/camera-subpackages PR notes.
