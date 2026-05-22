package main

// DefaultBatteryRules is the rule set the camera daemon falls back to
// when no operator-configured battery_rules.json exists on disk yet.
// Tuned for the off-grid solar deployment that motivated #73:
//
//   - At ≤ 20 % battery, drop to standby (recording continues, WHIP
//     publisher only opens when a viewer arrives) and switch the
//     uploader to lazy mode. Buys multiple extra hours of recording
//     headroom without going fully dark.
//   - At ≤ 10 % battery, drop to sleep (capture suspended, telemetry
//     every 5 min) so the radio stays available for an SMS wake and
//     the camera doesn't brown-out before the operator can react.
//
// A file containing `[]` is treated as "operator explicitly cleared
// rules" and overrides these defaults (see LoadBatteryRules /
// SaveBatteryRules contract). These defaults only apply on a fresh
// install where the file has never been written.
func DefaultBatteryRules() []BatteryRule {
	// Stored lowest-threshold-first to match the canonical form
	// SetBatteryRules sorts to and EvaluateBatteryRules expects.
	// Reading top-to-bottom: at 10 % → sleep first, then standby
	// activates at 20 % once the battery climbs back above 10.
	return []BatteryRule{
		{Threshold: 10, PowerMode: PowerModeSleep, UploadMode: "lazy"},
		{Threshold: 20, PowerMode: PowerModeStandby, UploadMode: "lazy"},
	}
}
