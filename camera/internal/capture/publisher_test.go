package capture

import (
	"bytes"
	"context"
	"os/exec"
	"testing"

	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// Tests for the H.264 access-unit boundary detection used by
// StreamH264. The whole point of #104 was that the old logic treated
// every VCL NAL as its own AU, so multi-slice frames were chopped into
// fragments the receiver couldn't reassemble. Cover the four cases
// that actually appear on the wire so a future "tidy this up" pass
// can't silently regress the boundary rules.

func TestIsSliceNAL(t *testing.T) {
	cases := []struct {
		name string
		t    h264reader.NalUnitType
		want bool
	}{
		{"non-IDR slice", h264reader.NalUnitTypeCodedSliceNonIdr, true},
		{"IDR slice", h264reader.NalUnitTypeCodedSliceIdr, true},
		{"data partition A", h264reader.NalUnitTypeCodedSliceDataPartitionA, true},
		{"data partition B", h264reader.NalUnitTypeCodedSliceDataPartitionB, true},
		{"data partition C", h264reader.NalUnitTypeCodedSliceDataPartitionC, true},
		{"SPS", h264reader.NalUnitTypeSPS, false},
		{"PPS", h264reader.NalUnitTypePPS, false},
		{"SEI", h264reader.NalUnitTypeSEI, false},
		{"AUD", h264reader.NalUnitTypeAUD, false},
	}
	for _, c := range cases {
		if got := isSliceNAL(c.t); got != c.want {
			t.Errorf("isSliceNAL(%v) = %v, want %v", c.name, got, c.want)
		}
	}
}

// === End-to-end tests for the AU-batching loop ============================
//
// These drive a real Annex-B bitstream through streamH264 and inspect the
// emitted media.Samples. The whole point of #104: a multi-slice picture
// must collapse into one Sample. A single-slice picture must still be one
// Sample. Parameter sets must ride along on the next AU rather than firing
// their own samples.

// startCode is the Annex-B picture-NAL prefix.
var startCode = []byte{0x00, 0x00, 0x00, 0x01}

// h264NAL builds an Annex-B-prefixed NAL unit. nalHeader is the first
// byte after the start code (top 3 bits forbidden/nri, low 5 bits the
// nal_unit_type). bodyFirstByte is the first byte of the slice header
// RBSP — its MSB encodes first_mb_in_slice (1 = first slice of a new
// picture, 0 = continuation slice). bodyTail is arbitrary filler so
// the NAL has nonzero length.
func h264NAL(nalHeader byte, bodyFirstByte byte, bodyTail ...byte) []byte {
	out := append([]byte{}, startCode...)
	out = append(out, nalHeader, bodyFirstByte)
	out = append(out, bodyTail...)
	return out
}

// collectSamples drains streamH264 against the given bitstream and
// returns the emitted samples. ctx is short-lived; the function reads
// to EOF synchronously.
func collectSamples(t *testing.T, stream []byte) []media.Sample {
	t.Helper()
	var got []media.Sample
	err := streamH264(context.Background(), bytes.NewReader(stream),
		func(s media.Sample) error {
			// Copy the data so a later mutation in the caller can't
			// invalidate the captured slice.
			cp := make([]byte, len(s.Data))
			copy(cp, s.Data)
			s.Data = cp
			got = append(got, s)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("streamH264: %v", err)
	}
	return got
}

// countNALsInAU counts how many start-code-prefixed NAL units appear
// in an emitted access unit's buffer.
func countNALsInAU(au []byte) int {
	return bytes.Count(au, startCode)
}

func TestStreamH264_SingleSliceFrames(t *testing.T) {
	// SPS + PPS + 3 P-frames, each one slice. Expected: one Sample per
	// frame, with SPS/PPS rolled into the first one.
	var stream []byte
	// SPS (type 7, nri=3 → 0x67). Body irrelevant for the AU splitter.
	stream = append(stream, h264NAL(0x67, 0x42, 0x00, 0x1f)...)
	// PPS (type 8 → 0x68).
	stream = append(stream, h264NAL(0x68, 0xeb, 0xef)...)
	// Frame 1: IDR slice (type 5 → 0x65), first_mb=0 (0x80).
	stream = append(stream, h264NAL(0x65, 0x80, 0x88, 0x84, 0x00)...)
	// Frame 2: non-IDR slice (type 1 → 0x41), first_mb=0.
	stream = append(stream, h264NAL(0x41, 0x80, 0x00)...)
	// Frame 3.
	stream = append(stream, h264NAL(0x41, 0x80, 0x00)...)

	samples := collectSamples(t, stream)
	if len(samples) != 3 {
		t.Fatalf("want 3 samples (frames), got %d", len(samples))
	}
	// AU 1 should carry SPS+PPS+slice (3 NALs).
	if got := countNALsInAU(samples[0].Data); got != 3 {
		t.Errorf("AU 1 expected 3 NALs (SPS+PPS+slice), got %d", got)
	}
	for i := 1; i < 3; i++ {
		if got := countNALsInAU(samples[i].Data); got != 1 {
			t.Errorf("AU %d expected 1 NAL, got %d", i+1, got)
		}
	}
}

func TestStreamH264_MultiSlicePerFrame(t *testing.T) {
	// Two pictures, each with two slices. This is the regression case
	// from #104: the old logic flushed on every VCL NAL after the first,
	// so each slice became its own Sample with marker=1 and the receiver
	// dropped the whole picture.
	var stream []byte
	stream = append(stream, h264NAL(0x67, 0x42)...) // SPS
	stream = append(stream, h264NAL(0x68, 0xeb)...) // PPS

	// Picture 1: IDR slice 1 (first_mb=0) + IDR slice 2 (first_mb>0 → 0x40 = single bit '010' = ue(v) of 1).
	stream = append(stream, h264NAL(0x65, 0x80, 0xaa, 0xbb)...)
	stream = append(stream, h264NAL(0x65, 0x40, 0xcc, 0xdd)...)

	// Picture 2: non-IDR slice 1 (first_mb=0) + non-IDR slice 2 (first_mb>0).
	stream = append(stream, h264NAL(0x41, 0x80, 0xee, 0xff)...)
	stream = append(stream, h264NAL(0x41, 0x40, 0x12, 0x34)...)

	samples := collectSamples(t, stream)
	if len(samples) != 2 {
		t.Fatalf("want 2 samples (pictures), got %d:\n%s", len(samples), debugSamples(samples))
	}
	// Picture 1 AU: SPS + PPS + slice1 + slice2 = 4 NALs.
	if got := countNALsInAU(samples[0].Data); got != 4 {
		t.Errorf("Picture 1 expected 4 NALs (SPS+PPS+slice1+slice2), got %d: %x", got, samples[0].Data)
	}
	// Picture 2 AU: slice1 + slice2 = 2 NALs.
	if got := countNALsInAU(samples[1].Data); got != 2 {
		t.Errorf("Picture 2 expected 2 NALs (slice1+slice2), got %d: %x", got, samples[1].Data)
	}

	// Sanity: both slice payloads must appear inside their picture's AU.
	// If the old (buggy) splitter were still in place, slice 2 would
	// have ridden in a separate sample and the slice 2 marker bytes
	// (0xcc 0xdd, 0x12 0x34) would NOT appear in samples[0]/[1].
	if !bytes.Contains(samples[0].Data, []byte{0xcc, 0xdd}) {
		t.Errorf("Picture 1 lost slice 2's body bytes — slice was split into a separate AU")
	}
	if !bytes.Contains(samples[1].Data, []byte{0x12, 0x34}) {
		t.Errorf("Picture 2 lost slice 2's body bytes — slice was split into a separate AU")
	}
}

func TestStreamH264_FourSlicesPerFrame(t *testing.T) {
	// Stress: 4 slices per picture (what -tune zerolatency + sliced-threads
	// on a 4-core machine actually produces). All four must coalesce.
	var stream []byte
	stream = append(stream, h264NAL(0x67, 0x42)...) // SPS
	stream = append(stream, h264NAL(0x68, 0xeb)...) // PPS

	// Picture 1: 4 slices. first_mb_in_slice values 0, 1, 2, 3.
	// ue(v) encodings:
	//   0 → '1'         → 0x80
	//   1 → '010'       → 0x40
	//   2 → '011'       → 0x60
	//   3 → '00100'     → 0x20
	stream = append(stream, h264NAL(0x65, 0x80, 0xa1)...)
	stream = append(stream, h264NAL(0x65, 0x40, 0xa2)...)
	stream = append(stream, h264NAL(0x65, 0x60, 0xa3)...)
	stream = append(stream, h264NAL(0x65, 0x20, 0xa4)...)

	samples := collectSamples(t, stream)
	if len(samples) != 1 {
		t.Fatalf("want 1 sample (1 picture), got %d:\n%s", len(samples), debugSamples(samples))
	}
	if got := countNALsInAU(samples[0].Data); got != 6 {
		t.Errorf("expected 6 NALs (SPS+PPS+4 slices), got %d", got)
	}
	for i, marker := range []byte{0xa1, 0xa2, 0xa3, 0xa4} {
		if !bytes.Contains(samples[0].Data, []byte{marker}) {
			t.Errorf("slice %d body byte 0x%x missing — slice was dropped", i+1, marker)
		}
	}
}

func TestStreamH264_NonVCLBetweenPictures(t *testing.T) {
	// SPS/PPS that arrive between pictures (mid-stream parameter-set
	// refresh) get appended to the previous picture's AU rather than
	// held for the next one. Decoders parse parameter sets by NAL type
	// and cache them globally, so this works fine in practice — the
	// receiver applies the cached SPS/PPS when it decodes picture 2.
	// rpicam-vid + --inline puts parameter sets BEFORE every IDR so
	// this code path is rare; the test just pins the behaviour so a
	// future refactor doesn't silently change it.
	var stream []byte
	stream = append(stream, h264NAL(0x65, 0x80, 0x01)...) // Picture 1.
	stream = append(stream, h264NAL(0x67, 0x42)...)       // Mid-stream SPS.
	stream = append(stream, h264NAL(0x68, 0xeb)...)       // Mid-stream PPS.
	stream = append(stream, h264NAL(0x65, 0x80, 0x02)...) // Picture 2 (first_mb=0).

	samples := collectSamples(t, stream)
	if len(samples) != 2 {
		t.Fatalf("want 2 samples, got %d:\n%s", len(samples), debugSamples(samples))
	}
	// AU 1: slice1 + SPS + PPS (parameter sets attach to prior AU).
	if got := countNALsInAU(samples[0].Data); got != 3 {
		t.Errorf("AU 1 expected 3 NALs (slice1+SPS+PPS), got %d", got)
	}
	// AU 2: just slice2.
	if got := countNALsInAU(samples[1].Data); got != 1 {
		t.Errorf("AU 2 expected 1 NAL (slice2), got %d", got)
	}
}

// TestStreamH264_RealMultiSliceFromX264 generates a real H.264 stream
// from x264 with sliced-threads=4 enabled (the exact ffmpeg flags that
// the #104 issue calls out as unsafe with the old splitter). The number
// of emitted samples must match the picture count, NOT the slice count
// — i.e. the splitter actually coalesces.
//
// Skipped when ffmpeg isn't on PATH (CI Pi images don't have it). Run
// locally to validate against the same encoder the camera will use in
// production once -tune zerolatency is re-enabled.
func TestStreamH264_RealMultiSliceFromX264(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH; skipping real-encoder test")
	}
	const frames = 12
	// Use testsrc2 → x264 with 4 slices per frame and -tune zerolatency
	// (which implies sliced-threads). -g 12 keeps every frame an IDR-or-P
	// keyframe so the splitter exercise covers the IDR + P paths.
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=12:duration=1",
		"-c:v", "libx264",
		"-tune", "zerolatency",
		"-x264-params", "slices=4:sliced-threads=1",
		"-g", "12",
		"-f", "h264",
		"-frames:v", "12",
		"pipe:1",
	)
	stream, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffmpeg generate: %v", err)
	}
	if len(stream) < 100 {
		t.Fatalf("ffmpeg produced suspiciously small output (%d bytes)", len(stream))
	}

	// Count slice NALs to confirm ffmpeg actually emitted multiple
	// slices per picture — if the encoder silently ignored the slices=4
	// hint, the test wouldn't be exercising anything new.
	reader, err := h264reader.NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("h264reader: %v", err)
	}
	var sliceCount int
	for {
		nal, err := reader.NextNAL()
		if err != nil {
			break
		}
		if isSliceNAL(nal.UnitType) {
			sliceCount++
		}
	}
	if sliceCount < frames*2 {
		t.Fatalf("expected multi-slice encoding (>= %d slice NALs for %d pictures), got %d",
			frames*2, frames, sliceCount)
	}

	samples := collectSamples(t, stream)
	if len(samples) != frames {
		t.Errorf("multi-slice splitter: want %d samples (1 per picture), got %d. "+
			"sliceCount=%d → splitter is still flushing per-slice instead of per-picture",
			frames, len(samples), sliceCount)
	}
	// Each sample must contain at least one slice NAL.
	for i, s := range samples {
		nals := countNALsInAU(s.Data)
		if nals < 1 {
			t.Errorf("AU %d empty (%d NALs)", i, nals)
		}
	}
}

func TestStreamH264_TrailingPictureFlushedOnEOF(t *testing.T) {
	// Last picture in the stream must still be emitted at EOF.
	stream := h264NAL(0x65, 0x80, 0xff)
	samples := collectSamples(t, stream)
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	if !bytes.Contains(samples[0].Data, []byte{0xff}) {
		t.Errorf("trailing picture's body was lost")
	}
}

func debugSamples(samples []media.Sample) string {
	var b bytes.Buffer
	for i, s := range samples {
		b.WriteString("  AU ")
		b.WriteString(intToString(i))
		b.WriteString(": ")
		for _, x := range s.Data {
			b.WriteString(byteToHex(x))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

func byteToHex(b byte) string {
	const h = "0123456789abcdef"
	return string([]byte{h[b>>4], h[b&0x0f], ' '})
}

func TestIsFirstSliceOfPicture(t *testing.T) {
	// data[0] is the NAL header byte; data[1] starts the slice header
	// RBSP. first_mb_in_slice is the first ue(v) value. Value 0 is
	// encoded as the single bit '1', so data[1] & 0x80 != 0.
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		// NAL header arbitrary (the function ignores it); only the
		// slice-header byte matters.
		{"first_mb_in_slice == 0 (MSB set)", []byte{0x65, 0x88, 0x00}, true},

		// Value 1 is ue(v) '010' → first byte 0x40 (MSB cleared,
		// next-MSB set, rest zero).
		{"first_mb_in_slice == 1 (MSB cleared)", []byte{0x65, 0x40, 0x00}, false},

		// Larger values also have MSB cleared (any value > 0 needs at
		// least one leading zero in the ue(v) coding).
		{"first_mb_in_slice large (MSB cleared)", []byte{0x65, 0x00, 0x80}, false},

		// Single-byte NAL: degenerate, treat as a fresh picture so we
		// don't silently swallow it into the previous AU.
		{"truncated NAL", []byte{0x65}, true},

		// Empty NAL: shouldn't happen, but defensively treat as new pic.
		{"empty data", []byte{}, true},
	}
	for _, c := range cases {
		if got := isFirstSliceOfPicture(c.data); got != c.want {
			t.Errorf("isFirstSliceOfPicture(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}
