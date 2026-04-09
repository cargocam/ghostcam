package camera

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
