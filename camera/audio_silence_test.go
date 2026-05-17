package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sampleVolumedetectStderr is real ffmpeg output captured from a
// volumedetect run. The parser must lock onto the mean_volume line
// even though plenty of other lines surround it.
const sampleVolumedetectStderr = `Input #0, mpegts, from 'seg00100.ts':
  Duration: 00:00:04.04, start: 1.400000, bitrate: 1234 kb/s
    Stream #0:0[0x100]: Video: h264 (High) ([27][0][0][0] / 0x001B), yuv420p
    Stream #0:1[0x101](und): Audio: aac (LC) ([15][0][0][0] / 0x000F), 48000 Hz, stereo
[Parsed_volumedetect_0 @ 0x55f5e8e08c40] n_samples: 387072
[Parsed_volumedetect_0 @ 0x55f5e8e08c40] mean_volume: -27.4 dB
[Parsed_volumedetect_0 @ 0x55f5e8e08c40] max_volume: -8.2 dB
[Parsed_volumedetect_0 @ 0x55f5e8e08c40] histogram_0db: 17
[Parsed_volumedetect_0 @ 0x55f5e8e08c40] histogram_1db: 152
size=       0kB time=00:00:04.04 bitrate=   0.0kbits/s speed= 218x
`

const silentStderr = `[Parsed_volumedetect_0 @ 0x000000] n_samples: 96000
[Parsed_volumedetect_0 @ 0x000000] mean_volume: -90.3 dB
[Parsed_volumedetect_0 @ 0x000000] max_volume: -66.8 dB
`

func TestParseMeanVolume(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  float32
	}{
		{"loud speech", sampleVolumedetectStderr, -27.4},
		{"digital silence (the #114 case)", silentStderr, -90.3},
		// Integer dB value (no decimal). ffmpeg sometimes rounds to an
		// integer for silence-floor measurements.
		{"integer dB", "mean_volume: -91 dB\n", -91.0},
		// Positive sign is unusual but documented as possible if the
		// signal clips — accept it.
		{"clipped near 0 dB", "mean_volume: 0.1 dB\n", 0.1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseMeanVolume(c.input)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			// Compare as float32 with a tolerance — string-to-float
			// is exact in our cases, but be lenient.
			if abs32(got-c.want) > 0.001 {
				t.Fatalf("want %v, got %v", c.want, got)
			}
		})
	}
}

func TestParseMeanVolume_NoMatch(t *testing.T) {
	cases := []string{
		"",
		"ffmpeg failed: no audio stream",
		"max_volume: -8 dB\n", // mean_volume missing — must not match
	}
	for _, in := range cases {
		if _, err := ParseMeanVolume(in); err == nil {
			t.Fatalf("expected error for input %q", in)
		}
	}
}

func TestPickSegmentForMeasurement(t *testing.T) {
	dir := t.TempDir()

	// No segments at all — return "".
	if got, err := pickSegmentForMeasurement(dir); err != nil || got != "" {
		t.Fatalf("empty dir: want (\"\", nil), got (%q, %v)", got, err)
	}

	// One segment — still nothing to pick (we want the SECOND-newest).
	one := filepath.Join(dir, "seg00001.ts")
	if err := os.WriteFile(one, nil, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, err := pickSegmentForMeasurement(dir); err != nil || got != "" {
		t.Fatalf("one segment: want (\"\", nil), got (%q, %v)", got, err)
	}

	// Two segments, both fresh — the second-newest (seg00001) is too
	// fresh to be safe yet.
	two := filepath.Join(dir, "seg00002.ts")
	if err := os.WriteFile(two, nil, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, err := pickSegmentForMeasurement(dir); err != nil || got != "" {
		t.Fatalf("two fresh segments: want (\"\", nil), got (%q, %v)", got, err)
	}

	// Backdate seg00001 past the age gate; seg00002 is the newest
	// (current write target) so we want seg00001.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(one, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	got, err := pickSegmentForMeasurement(dir)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got != one {
		t.Fatalf("want %q, got %q", one, got)
	}

	// Add seg00003 (newest); now seg00002 is second-newest. Backdate
	// it to make it eligible.
	three := filepath.Join(dir, "seg00003.ts")
	if err := os.WriteFile(three, nil, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(two, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	got, err = pickSegmentForMeasurement(dir)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got != two {
		t.Fatalf("after adding newer segment, want %q, got %q", two, got)
	}
}

func TestReadAudioRMSDBFS_RoundTrip(t *testing.T) {
	ResetAudioRMSDBFSForTest()
	if got := ReadAudioRMSDBFS(); got != nil {
		t.Fatalf("expected nil before any write, got %v", *got)
	}
	SetAudioRMSDBFS(-42.5)
	got := ReadAudioRMSDBFS()
	if got == nil {
		t.Fatal("expected a value after write")
	}
	if abs32(*got-(-42.5)) > 0.001 {
		t.Fatalf("want -42.5, got %v", *got)
	}
}

func abs32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}
