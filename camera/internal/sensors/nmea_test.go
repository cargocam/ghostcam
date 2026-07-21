package sensors

import (
	"fmt"
	"math"
	"testing"
)

func approx(a, b, eps float64) bool { return math.Abs(a-b) < eps }

// nmeaLine appends the correct *HH XOR checksum to a sentence body that
// starts with '$' (e.g. "$GPGGA,..."), so tests assert parsing behavior
// rather than hand-computed checksum arithmetic.
func nmeaLine(body string) string {
	var sum byte
	for i := 1; i < len(body); i++ { // skip leading '$'
		sum ^= body[i]
	}
	return fmt.Sprintf("%s*%02X", body, sum)
}

func TestParseCoord(t *testing.T) {
	tests := []struct {
		val, hemi string
		want      float64
		ok        bool
	}{
		{"4807.038", "N", 48.1173, true},   // 48°07.038'
		{"01131.000", "E", 11.51667, true}, // 011°31.000'
		{"4807.038", "S", -48.1173, true},
		{"01131.000", "W", -11.51667, true},
		{"", "N", 0, false},
		{"4807.038", "", 0, false},
		{"notnum", "N", 0, false},
		{"4807.038", "X", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseCoord(tc.val, tc.hemi)
		if ok != tc.ok {
			t.Errorf("parseCoord(%q,%q) ok=%v want %v", tc.val, tc.hemi, ok, tc.ok)
			continue
		}
		if ok && !approx(got, tc.want, 1e-4) {
			t.Errorf("parseCoord(%q,%q)=%f want %f", tc.val, tc.hemi, got, tc.want)
		}
	}
}

func TestVerifyNMEAChecksum(t *testing.T) {
	// Round-trip: a line built with the correct checksum must verify.
	for _, body := range []string{
		"$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,",
		"$GNRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W",
	} {
		if l := nmeaLine(body); !verifyNMEAChecksum(l) {
			t.Errorf("verifyNMEAChecksum(%q) = false, want true", l)
		}
	}
	invalid := []string{
		"$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*00", // wrong checksum
		"$GPGGA,123519,4807.038,N",                                          // no checksum
		"garbage",
		"",
		"$GPGGA*ZZ", // non-hex checksum
	}
	for _, l := range invalid {
		if verifyNMEAChecksum(l) {
			t.Errorf("verifyNMEAChecksum(%q) = true, want false", l)
		}
	}
}

func TestParserGGAWithGSA3D(t *testing.T) {
	var p nmeaParser
	if _, ok := p.feed(nmeaLine("$GPGSA,A,3,04,05,,09,12,,,24,,,,,2.5,1.3,2.1")); ok {
		t.Fatal("GSA should not emit a fix")
	}
	fix, ok := p.feed(nmeaLine("$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,"))
	if !ok {
		t.Fatal("fixed GGA should emit a fix")
	}
	if !approx(fix.lat, 48.1173, 1e-4) || !approx(fix.lon, 11.51667, 1e-4) {
		t.Errorf("position = %f,%f want ~48.1173,11.51667", fix.lat, fix.lon)
	}
	if !approx(float64(fix.alt), 545.4, 0.1) {
		t.Errorf("alt = %f want 545.4", fix.alt)
	}
	if fix.mode != 3 {
		t.Errorf("mode = %d want 3 (from GSA)", fix.mode)
	}
}

func TestParserGGA2D(t *testing.T) {
	var p nmeaParser
	p.feed(nmeaLine("$GPGSA,A,2,04,05,,,,,,,,,,,2.5,1.3,2.1"))
	fix, ok := p.feed(nmeaLine("$GPGGA,123519,4807.038,N,01131.000,E,2,08,0.9,10.0,M,46.9,M,,"))
	if !ok {
		t.Fatal("DGPS-quality GGA should emit a fix")
	}
	if fix.mode != 2 {
		t.Errorf("mode = %d want 2 (from GSA 2D)", fix.mode)
	}
}

func TestParserNoGSADefaultsTo3D(t *testing.T) {
	var p nmeaParser
	fix, ok := p.feed(nmeaLine("$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,"))
	if !ok {
		t.Fatal("fixed GGA should emit a fix even without prior GSA")
	}
	if fix.mode != 3 {
		t.Errorf("mode = %d want 3 (default when no GSA seen)", fix.mode)
	}
}

func TestParserNoFixGGARejected(t *testing.T) {
	var p nmeaParser
	if _, ok := p.feed(nmeaLine("$GPGGA,123519,,,,,0,00,,,M,,M,,")); ok {
		t.Error("GGA with quality 0 should not emit a fix")
	}
}

func TestParserGarbledRejected(t *testing.T) {
	var p nmeaParser
	// Valid body but corrupted checksum (line noise on the serial link).
	good := nmeaLine("$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,")
	bad := good[:len(good)-2] + "11" // clobber the two checksum hex digits
	if bad == good {
		t.Fatal("test setup: checksum digits happened to be 11; adjust")
	}
	if _, ok := p.feed(bad); ok {
		t.Error("GGA with bad checksum should be rejected")
	}
}

func TestParserIgnoresOtherTalkers(t *testing.T) {
	var p nmeaParser
	// GN talker (multi-constellation) must be accepted like GP.
	fix, ok := p.feed(nmeaLine("$GNGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,"))
	if !ok {
		t.Fatal("GNGGA should be parsed like GPGGA")
	}
	if !approx(fix.lat, 48.1173, 1e-4) {
		t.Errorf("lat = %f want ~48.1173", fix.lat)
	}
}
