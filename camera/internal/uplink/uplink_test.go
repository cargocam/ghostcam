package uplink

import "testing"

func TestDecideForce(t *testing.T) {
	const now = 1_000_000
	tests := []struct {
		name  string
		until int64
		want  forceAction
	}{
		{"no deadline set", 0, actIdle},
		{"deadline in the future", now + 5000, actForce},
		{"deadline just passed", now - 1, actRestore},
		{"deadline exactly now is not future", now, actRestore},
		{"far future", now + 1_000_000, actForce},
	}
	for _, tc := range tests {
		if got := decideForce(tc.until, now); got != tc.want {
			t.Errorf("%s: decideForce(%d,%d)=%v want %v", tc.name, tc.until, now, got, tc.want)
		}
	}
}

func TestClampForceSeconds(t *testing.T) {
	tests := []struct{ in, want int }{
		{-5, 0},
		{0, 0},
		{60, 60},
		{ForceCellularMaxSeconds, ForceCellularMaxSeconds},
		{ForceCellularMaxSeconds + 1, ForceCellularMaxSeconds},
		{999999, ForceCellularMaxSeconds},
	}
	for _, tc := range tests {
		if got := ClampForceSeconds(tc.in); got != tc.want {
			t.Errorf("ClampForceSeconds(%d)=%d want %d", tc.in, got, tc.want)
		}
	}
}
