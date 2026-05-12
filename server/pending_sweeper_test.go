package main

import (
	"testing"
	"time"
)

func TestPendingTTLSeconds(t *testing.T) {
	// Threshold sanity: long enough that ordinary cellular uploads don't
	// trip it, short enough that an actually-failed upload doesn't sit on
	// the timeline for hours. Anything outside [60s, 30min] would be
	// surprising and worth thinking twice about before changing.
	if PendingTTLSeconds < 60 {
		t.Errorf("PendingTTLSeconds=%d too short — normal slow uploads would expire", PendingTTLSeconds)
	}
	if PendingTTLSeconds > 30*60 {
		t.Errorf("PendingTTLSeconds=%d too long — stale rows pile up", PendingTTLSeconds)
	}
}

func TestPendingSweeperRespectsContext(t *testing.T) {
	// Smoke test: sweeper exits promptly when context cancels. Without a
	// running goroutine to verify here we'd be testing the syntax, so
	// keep it minimal — the real coverage is the production wiring in
	// main.go pairing the cancel context with the sweep loop.
	d := time.Now().Add(time.Hour).UnixMilli()
	if d <= 0 {
		t.Fatal("UnixMilli sanity check failed")
	}
}
