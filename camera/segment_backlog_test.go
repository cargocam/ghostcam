package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSampleLocalSegmentBacklog_Unseeded(t *testing.T) {
	// Make sure the atomic is empty for this test. Other tests in the
	// package may seed it; explicitly clear here.
	segmentDirForTelemetry.Store(nil)
	if _, ok := SampleLocalSegmentBacklog(); ok {
		t.Errorf("unseeded sampler returned ok=true")
	}
}

func TestSampleLocalSegmentBacklog_CountsM4sOnly(t *testing.T) {
	dir := t.TempDir()
	// Drop a mix of files: 3 .m4s segments, init.mp4 (which should be
	// excluded), playlist.m3u8, a pending_confirms.json. Only .m4s
	// counts toward the backlog.
	for _, name := range []string{"seg00001.m4s", "seg00002.m4s", "seg00003.m4s"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	for _, name := range []string{"init.mp4", "playlist.m3u8", "pending_confirms.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	SetSegmentDirForTelemetry(dir)
	t.Cleanup(func() { segmentDirForTelemetry.Store(nil) })

	n, ok := SampleLocalSegmentBacklog()
	if !ok {
		t.Fatal("sampler reported ok=false on seeded dir")
	}
	if n != 3 {
		t.Errorf("counted %d, want 3", n)
	}
}

func TestSampleLocalSegmentBacklog_BadDir(t *testing.T) {
	SetSegmentDirForTelemetry("/this/dir/never/exists/abc")
	t.Cleanup(func() { segmentDirForTelemetry.Store(nil) })
	if _, ok := SampleLocalSegmentBacklog(); ok {
		t.Errorf("non-existent dir returned ok=true")
	}
}

func TestSampleLocalSegmentBacklog_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	SetSegmentDirForTelemetry(dir)
	t.Cleanup(func() { segmentDirForTelemetry.Store(nil) })
	n, ok := SampleLocalSegmentBacklog()
	if !ok {
		t.Fatal("empty dir reported ok=false")
	}
	if n != 0 {
		t.Errorf("counted %d on empty dir, want 0", n)
	}
}
