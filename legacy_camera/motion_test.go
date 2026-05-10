package main

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

	// Window should have evicted old entries; avg is still ~100000
	if !md.detect("", 160000) {
		t.Error("1.6x spike after full window should trigger motion")
	}
}

func TestIsValidTS(t *testing.T) {
	t.Run("nonexistent file", func(t *testing.T) {
		if isValidTS("/tmp/ghostcam-nonexistent-file.ts") {
			t.Error("nonexistent file should not be valid")
		}
	})

	t.Run("valid TS sync byte", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "valid.ts")
		os.WriteFile(path, []byte{0x47, 0x00, 0x11, 0x00}, 0644)
		if !isValidTS(path) {
			t.Error("file starting with 0x47 should be valid TS")
		}
	})

	t.Run("invalid first byte", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid.ts")
		os.WriteFile(path, []byte{0x00, 0x47, 0x00, 0x00}, 0644)
		if isValidTS(path) {
			t.Error("file not starting with 0x47 should be invalid TS")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.ts")
		os.WriteFile(path, []byte{}, 0644)
		if isValidTS(path) {
			t.Error("empty file should not be valid")
		}
	})
}
