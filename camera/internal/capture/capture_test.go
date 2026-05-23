package capture

import (
	"testing"

	"github.com/cargocam/ghostcam/camera/internal/state"
)

// TestResolveCaptureVideoParams was moved here from
// internal/abr/abr_test.go during the camera/ → internal/* subpackage
// split. It exercises capture's tier-override contract, not the ABR
// controller itself, so it belongs alongside the function it tests.
func TestResolveCaptureVideoParams(t *testing.T) {
	cfg := &state.CameraConfig{VideoWidth: 1280, VideoHeight: 720, VideoBitrate: 2_000_000}

	// No tier: cfg defaults pass through unchanged.
	w, h, br := resolveCaptureVideoParams(cfg, nil)
	if w != 1280 || h != 720 || br != 2_000_000 {
		t.Errorf("nil tier should preserve cfg defaults; got %d×%d @ %d", w, h, br)
	}

	// Tier override wins. This is the contract that lets ABR mutate
	// rpicam-vid params without touching the shared cfg struct.
	tier := &state.ABRTier{Name: "minimum", Width: 854, Height: 480, Bitrate: 500_000}
	w, h, br = resolveCaptureVideoParams(cfg, tier)
	if w != 854 || h != 480 || br != 500_000 {
		t.Errorf("tier override broken; got %d×%d @ %d, want 854×480 @ 500000", w, h, br)
	}

	// cfg must NOT have been mutated by the override.
	if cfg.VideoWidth != 1280 || cfg.VideoHeight != 720 || cfg.VideoBitrate != 2_000_000 {
		t.Errorf("cfg unexpectedly mutated by tier override: %+v", cfg)
	}
}
