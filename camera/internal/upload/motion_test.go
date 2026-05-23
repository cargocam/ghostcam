package upload

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMotionDetector_FileSizeFallback(t *testing.T) {
	md := newMotionDetector()

	// First 3 segments: warmup, never flags motion
	for i := 0; i < 3; i++ {
		if md.detect("", 100000) {
			t.Errorf("segment %d should not be flagged as motion (warmup)", i)
		}
	}

	// Consistent sizes — no motion
	if md.detect("", 100000) {
		t.Error("consistent size should not trigger motion")
	}

	// 2x spike — should trigger motion
	if !md.detect("", 200000) {
		t.Error("2x size spike should trigger motion")
	}

	// Back to normal
	if md.detect("", 100000) {
		t.Error("return to normal should not trigger motion")
	}
}

func TestMotionDetector_RollingWindow(t *testing.T) {
	md := newMotionDetector()

	// Fill beyond the window (max 10)
	for i := 0; i < 12; i++ {
		md.detect("", 100000)
	}

	// Window should have evicted old entries; avg is still ~100000.
	// Threshold is 1.8x so a 2x spike clears it.
	if !md.detect("", 200000) {
		t.Error("2x spike after full window should trigger motion")
	}
}

func TestIsValidFMP4Segment(t *testing.T) {
	t.Run("nonexistent file", func(t *testing.T) {
		if isValidFMP4Segment("/tmp/ghostcam-nonexistent-file.m4s") {
			t.Error("nonexistent file should not be valid")
		}
	})

	t.Run("valid styp marker", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "valid.m4s")
		// 4-byte size + "styp" type — minimal segment-type box header.
		os.WriteFile(path, []byte{0x00, 0x00, 0x00, 0x18, 's', 't', 'y', 'p'}, 0644)
		if !isValidFMP4Segment(path) {
			t.Error("file with styp marker should be valid")
		}
	})

	t.Run("ftyp (init segment, not media)", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "init.mp4")
		os.WriteFile(path, []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p'}, 0644)
		if isValidFMP4Segment(path) {
			t.Error("ftyp (init) marker should not pass media-segment check")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.m4s")
		os.WriteFile(path, []byte{}, 0644)
		if isValidFMP4Segment(path) {
			t.Error("empty file should not be valid")
		}
	})
}
