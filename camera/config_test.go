package main

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

// TestDefaultLocalStorageCapBytes_* moved here from
// internal/upload/upload_test.go during the subpackage split:
// defaultLocalStorageCapBytes lives in config.go (package main) and
// can only be exercised from the same package.
func TestDefaultLocalStorageCapBytes_ClampsCeiling(t *testing.T) {
	// A dir that exists (any tempdir is fine) should return a real
	// total; whatever it is, the cap respects the 4 GB ceiling. On a
	// dev box / CI runner the host disk is generally > 8 GB so half >
	// 4 GB and we expect the ceiling. On a tiny container the floor
	// branch kicks in instead. Both are valid outcomes — verify only
	// that the result respects the documented bounds.
	const (
		ceiling = uint64(4 * 1024 * 1024 * 1024)
		floor   = uint64(256 * 1024 * 1024)
	)
	dir := t.TempDir()
	got := defaultLocalStorageCapBytes(dir)
	if got < floor || got > ceiling {
		t.Errorf("default cap %d not in [256 MB, 4 GB]", got)
	}
}

func TestDefaultLocalStorageCapBytes_FallsBackOnBadDir(t *testing.T) {
	// A path that doesn't exist falls back to /, which always
	// statfs's successfully on a real host. So the cap ends up
	// derived from the host disk (not the fallback ceiling). Just
	// check that we get a sane value, not the explicit fallback.
	got := defaultLocalStorageCapBytes("/this/path/never/exists/abc123")
	const (
		ceiling = uint64(4 * 1024 * 1024 * 1024)
		floor   = uint64(256 * 1024 * 1024)
	)
	if got < floor || got > ceiling {
		t.Errorf("walk-up cap %d not in [256 MB, 4 GB]", got)
	}
}
