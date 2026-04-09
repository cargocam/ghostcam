package handlers

import "testing"

func TestParseStreamIDTimestamp(t *testing.T) {
	// parseStreamIDTimestamp is in events.go via redis package, but
	// epochMsToISO8601 is testable here
	tests := []struct {
		epochMs uint64
		want    string
	}{
		{0, "1970-01-01T00:00:00.000Z"},
		{1775401947875, "2026-04-05T15:12:27.875Z"},
		{1775538068000, "2026-04-07T05:01:08.000Z"},
	}

	for _, tt := range tests {
		got := epochMsToISO8601(tt.epochMs)
		if got != tt.want {
			t.Errorf("epochMsToISO8601(%d) = %q, want %q", tt.epochMs, got, tt.want)
		}
	}
}
