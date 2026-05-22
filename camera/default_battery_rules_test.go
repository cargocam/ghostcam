package main

import "testing"

func TestDefaultBatteryRules_ShapeAndOrdering(t *testing.T) {
	rules := DefaultBatteryRules()

	if len(rules) != 2 {
		t.Fatalf("expected 2 default rules, got %d", len(rules))
	}
	// Caller (main.go) hands the slice to SetBatteryRules, which sorts
	// it. The defaults themselves should already be sorted lowest-
	// threshold-first so anyone iterating the literal value gets the
	// most-aggressive-first ordering EvaluateBatteryRules expects.
	if rules[0].Threshold >= rules[1].Threshold {
		t.Errorf("expected defaults sorted lowest-threshold-first, got %d, %d",
			rules[0].Threshold, rules[1].Threshold)
	}

	// Smoke-check that the two rules are exactly the documented solar
	// defaults — if you intend to change these, update the comment in
	// default_battery_rules.go too.
	if rules[0].Threshold != 10 || rules[0].PowerMode != PowerModeSleep || rules[0].UploadMode != "lazy" {
		t.Errorf("unexpected rule 0: %+v", rules[0])
	}
	if rules[1].Threshold != 20 || rules[1].PowerMode != PowerModeStandby || rules[1].UploadMode != "lazy" {
		t.Errorf("unexpected rule 1: %+v", rules[1])
	}
}

func TestDefaultBatteryRules_EvaluatesCorrectly(t *testing.T) {
	// Sanity check: round-tripping the defaults through the same
	// SetBatteryRules → CurrentBatteryRules → EvaluateBatteryRules
	// chain main.go uses yields the expected mode at every threshold
	// boundary. Reset the global rule set on exit so later tests
	// (e.g. power_mode_test.go's set_power_mode no-op check) don't
	// observe stale rules from this test.
	t.Cleanup(func() { SetBatteryRules(nil) })
	SetBatteryRules(DefaultBatteryRules())
	sorted := CurrentBatteryRules()

	cases := []struct {
		pct  uint8
		want string // "" = no rule fires
	}{
		{100, ""},
		{50, ""},
		{21, ""},
		{20, PowerModeStandby},
		{15, PowerModeStandby},
		{11, PowerModeStandby},
		{10, PowerModeSleep},
		{5, PowerModeSleep},
		{0, PowerModeSleep},
	}
	for _, tc := range cases {
		pct := tc.pct
		got := EvaluateBatteryRules(sorted, &pct)
		if tc.want == "" {
			if got != nil {
				t.Errorf("pct=%d: expected no rule, got %+v", pct, got)
			}
			continue
		}
		if got == nil || got.PowerMode != tc.want {
			t.Errorf("pct=%d: expected mode=%q, got %+v", pct, tc.want, got)
		}
	}
}
