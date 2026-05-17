package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
)

// BatteryRule is a per-camera override that fires when battery_pct
// falls AT OR BELOW Threshold. The LOWEST-threshold rule that matches
// wins (most aggressive applicable), so a ladder like
// [{20, standby, lazy}, {10, sleep, lazy}] picks `sleep` at 8 % and
// `standby` at 15 %. This matches the precedence documented on the
// editor side (ui/.../BatteryRulesEditorDialog.svelte).
//
// Level-triggered (not edge-triggered): when the battery climbs back
// above every threshold, no rule fires and the camera reverts to
// whatever manual mode the operator last set via set_power_mode. The
// UploadMode field is wire-only for now — Go-side upload_mode handling
// is tracked separately; we round-trip the field so the UI's
// BatteryRulesEditorDialog can save it but the camera won't act on it.
type BatteryRule struct {
	Threshold  uint8  `json:"threshold_pct"`
	PowerMode  string `json:"power_mode"`
	UploadMode string `json:"upload_mode,omitempty"`
}

const batteryRulesFilename = "battery_rules.json"

var currentBatteryRules atomic.Pointer[[]BatteryRule]

// SetBatteryRules replaces the active rule set. Rules are sorted
// lowest-threshold first so EvaluateBatteryRules can pick the first
// match in O(n) and that match is the most-aggressive applicable rule.
func SetBatteryRules(rules []BatteryRule) {
	sorted := append([]BatteryRule(nil), rules...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Threshold < sorted[j].Threshold
	})
	currentBatteryRules.Store(&sorted)
}

// CurrentBatteryRules returns the active rule set (already sorted
// lowest-threshold first). Returns nil when no rules are configured.
func CurrentBatteryRules() []BatteryRule {
	if p := currentBatteryRules.Load(); p != nil {
		return *p
	}
	return nil
}

// EvaluateBatteryRules returns the rule that should fire for the given
// pct, or nil if no rule matches or pct is unavailable. Caller must
// pass rules already sorted lowest-threshold first (SetBatteryRules
// does this) — the first rule whose threshold meets or exceeds pct is
// the most-aggressive applicable rule.
func EvaluateBatteryRules(rules []BatteryRule, pct *uint8) *BatteryRule {
	if pct == nil {
		return nil
	}
	for i := range rules {
		if *pct <= rules[i].Threshold {
			return &rules[i]
		}
	}
	return nil
}

// LoadBatteryRules reads {dataDir}/battery_rules.json into the active
// rule set. Missing file = no rules configured (the default state for
// any camera the operator hasn't customised). Unparseable file is
// logged via the returned error so the caller can decide whether to
// surface it; callers that want best-effort behaviour can ignore the
// error.
func LoadBatteryRules(dataDir string) ([]BatteryRule, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, batteryRulesFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read battery rules: %w", err)
	}
	var rules []BatteryRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("parse battery rules: %w", err)
	}
	return rules, nil
}

// SaveBatteryRules atomically persists rules to {dataDir}/battery_rules.json.
// An empty / nil rule list still writes the file as `[]` so a later
// LoadBatteryRules call distinguishes "operator explicitly cleared
// rules" from "never had any".
func SaveBatteryRules(dataDir string, rules []BatteryRule) error {
	if rules == nil {
		rules = []BatteryRule{}
	}
	data, err := json.Marshal(rules)
	if err != nil {
		return fmt.Errorf("marshal battery rules: %w", err)
	}
	path := filepath.Join(dataDir, batteryRulesFilename)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write battery rules: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename battery rules: %w", err)
	}
	return nil
}
