package camera

import "testing"

func TestCoalesceStr(t *testing.T) {
	tests := []struct {
		vals []string
		want string
	}{
		{[]string{}, ""},
		{[]string{""}, ""},
		{[]string{"", "", "c"}, "c"},
		{[]string{"a", "b", "c"}, "a"},
		{[]string{"", "b"}, "b"},
	}
	for _, tt := range tests {
		got := coalesceStr(tt.vals...)
		if got != tt.want {
			t.Errorf("coalesceStr(%v) = %q, want %q", tt.vals, got, tt.want)
		}
	}
}

func TestResolveVideoProfile(t *testing.T) {
	tests := []struct {
		profile        string
		wantW, wantH   uint32
		wantBR, wantKF uint32
	}{
		{"zero2w", 854, 480, 750_000, 30},
		{"480p", 854, 480, 750_000, 30},
		{"pi4", 1280, 720, 2_000_000, 30},
		{"720p", 1280, 720, 2_000_000, 30},
		{"pi5", 1920, 1080, 4_000_000, 30},
		{"1080p", 1920, 1080, 4_000_000, 30},
		{"", 0, 0, 0, 0},
		{"unknown", 0, 0, 0, 0},
	}
	for _, tt := range tests {
		w, h, br, kf := resolveVideoProfile(tt.profile)
		if w != tt.wantW || h != tt.wantH || br != tt.wantBR || kf != tt.wantKF {
			t.Errorf("resolveVideoProfile(%q) = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
				tt.profile, w, h, br, kf, tt.wantW, tt.wantH, tt.wantBR, tt.wantKF)
		}
	}
}

func TestTrimString(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"  hello  ", "hello"},
		{"\n\thello\n\t", "hello"},
		{"", ""},
		{"  ", ""},
	}
	for _, tt := range tests {
		got := trimString(tt.in)
		if got != tt.want {
			t.Errorf("trimString(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
