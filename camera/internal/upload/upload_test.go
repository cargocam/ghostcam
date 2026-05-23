package upload

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cargocam/ghostcam/common"
)

func TestPendingConfirms_SaveLoad(t *testing.T) {
	dir := t.TempDir()

	// Empty load returns nil
	if got := loadPendingConfirms(dir); got != nil {
		t.Errorf("empty dir should return nil, got %v", got)
	}

	// Save some confirms
	confirms := []common.UploadedSegment{
		{SegmentID: "seg-1", StartTS: 1000, EndTS: 2000, SizeBytes: 500, HasMotion: false},
		{SegmentID: "seg-2", StartTS: 2000, EndTS: 3000, SizeBytes: 600, HasMotion: true},
	}
	savePendingConfirms(dir, confirms)

	// Load them back
	loaded := loadPendingConfirms(dir)
	if len(loaded) != 2 {
		t.Fatalf("expected 2 confirms, got %d", len(loaded))
	}
	if loaded[0].SegmentID != "seg-1" {
		t.Errorf("first segment ID = %q, want %q", loaded[0].SegmentID, "seg-1")
	}
	if !loaded[1].HasMotion {
		t.Error("second segment should have motion=true")
	}

	// Save nil clears the file
	savePendingConfirms(dir, nil)
	loaded = loadPendingConfirms(dir)
	if loaded != nil {
		t.Errorf("after saving nil, expected nil, got %v", loaded)
	}
}

func TestPendingConfirms_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending_confirms.json")
	os.WriteFile(path, []byte("not json{{{"), 0600)

	loaded := loadPendingConfirms(dir)
	if loaded != nil {
		t.Errorf("corrupt file should return nil, got %v", loaded)
	}
}

func TestPendingConfirms_EmptyDataDir(t *testing.T) {
	// Empty string dataDir should not panic
	savePendingConfirms("", nil)
	loaded := loadPendingConfirms("")
	if loaded != nil {
		t.Errorf("empty dataDir should return nil, got %v", loaded)
	}
}

// TestPendingConfirms_ENOSPCRateLimit verifies that #115's ENOSPC
// path: (a) sets inENOSPCState, (b) emits no panic, (c) the rate-
// limiter only allows one warn per enospcWarnIntervalSec, and (d) a
// later successful save clears the state. The save path goes through
// a non-writable dir to synthesize a write error that *isn't*
// strictly ENOSPC but exercises the same branch — true ENOSPC is hard
// to provoke in a sandbox without filling /tmp.
func TestPendingConfirms_ENOSPCRecoveryFlag(t *testing.T) {
	// Reset package state so prior tests don't bias this one.
	inENOSPCState.Store(false)
	lastENOSPCWarnUnix.Store(0)

	// noteENOSPC is the package-private rate-limited warner. Hit it
	// directly — calling savePendingConfirms with a bogus dir would
	// also trigger a non-ENOSPC warn branch, which is unrelated.
	noteENOSPC()
	if !inENOSPCState.Load() {
		t.Errorf("noteENOSPC didn't set inENOSPCState")
	}
	first := lastENOSPCWarnUnix.Load()
	if first == 0 {
		t.Errorf("noteENOSPC didn't bump lastENOSPCWarnUnix")
	}

	// Second call within the rate-limit window: should NOT advance
	// the timestamp (the CAS race is fine to ignore here because we
	// only call from this goroutine).
	noteENOSPC()
	if lastENOSPCWarnUnix.Load() != first {
		t.Errorf("noteENOSPC re-warned inside the rate-limit window")
	}

	// A successful savePendingConfirms after recovery flips
	// inENOSPCState back to false and (per the source) emits the
	// "recovered" info log.
	dir := t.TempDir()
	savePendingConfirms(dir, nil)
	if inENOSPCState.Load() {
		t.Errorf("successful save didn't clear inENOSPCState")
	}
}

// TestDefaultLocalStorageCapBytes_* moved to camera/config_test.go
// alongside their subject (defaultLocalStorageCapBytes lives in
// camera/config.go, package main).
