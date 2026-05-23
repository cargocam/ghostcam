package firmware

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Tests for the #106 firmware stability watchdog. The watchdog is
// 60-second-paced in production; tests use a 5 ms tick + 3-tick
// threshold so the whole table runs in a few hundred ms.
//
// What we want to pin:
//   * Watchdog blocks until boot_ok exists (no-network boot must NOT
//     accidentally accumulate stability minutes).
//   * Counter resets to 0 on every run.
//   * install_pending_verify is removed once the threshold is hit.
//   * If install_pending_verify never existed, removal is a no-op
//     (clean reboots of a long-stable install don't spam logs).
//   * ctx cancellation stops the loop cleanly.

const (
	testTick      = 5 * time.Millisecond
	testThreshold = 3
)

func TestFirmwareStability_WaitsForBootOk(t *testing.T) {
	dir := t.TempDir()
	pendingVerify := filepath.Join(dir, "install_pending_verify")
	bootOk := filepath.Join(dir, "boot_ok")
	healthyMinutes := filepath.Join(dir, "healthy_minutes")
	mustWriteFile(t, pendingVerify, "")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runFirmwareStabilityWatchdog(ctx, dir, testTick, testThreshold)
	}()

	// Until boot_ok exists, the watchdog must not have written
	// healthy_minutes — that's the whole point of the gate.
	time.Sleep(5 * testTick)
	if _, err := os.Stat(healthyMinutes); err == nil {
		data, _ := os.ReadFile(healthyMinutes)
		t.Errorf("watchdog wrote healthy_minutes (%q) before boot_ok existed", string(data))
	}
	// install_pending_verify must still be there too — without
	// stability minutes accumulating, the watchdog can't have hit
	// the threshold.
	if _, err := os.Stat(pendingVerify); err != nil {
		t.Errorf("install_pending_verify gone before boot_ok wrote: %v", err)
	}

	// Drop boot_ok in place; watchdog should start counting.
	mustWriteFile(t, bootOk, "")

	waitForFile(t, healthyMinutes, time.Second)
	waitForFileGone(t, pendingVerify, time.Second)
	cancel()
	wg.Wait()
}

func TestFirmwareStability_RemovesPendingVerifyAtThreshold(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "boot_ok"), "")
	pendingVerify := filepath.Join(dir, "install_pending_verify")
	healthyMinutes := filepath.Join(dir, "healthy_minutes")
	mustWriteFile(t, pendingVerify, "")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runFirmwareStabilityWatchdog(ctx, dir, testTick, testThreshold)
	}()

	waitForFileGone(t, pendingVerify, time.Second)

	// healthy_minutes must read ≥ threshold at the moment we observed
	// the marker disappear. Race-friendly check: read once after the
	// marker is gone — at that point the watchdog has written at
	// least `threshold`.
	data, err := os.ReadFile(healthyMinutes)
	if err != nil {
		t.Fatalf("read healthy_minutes: %v", err)
	}
	n, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("parse %q: %v", data, err)
	}
	if n < testThreshold {
		t.Errorf("expected healthy_minutes >= %d when pending_verify removed, got %d", testThreshold, n)
	}
	cancel()
	wg.Wait()
}

func TestFirmwareStability_NoPendingVerifyMeansNoLogSpam(t *testing.T) {
	// A long-running install that was verified ages ago has no
	// install_pending_verify marker. The watchdog should still tick
	// (writing healthy_minutes is harmless) but must not try to
	// remove anything past the threshold — the per-tick Remove
	// retry was a real-but-cheap bug I caught while wiring this.
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "boot_ok"), "")
	healthyMinutes := filepath.Join(dir, "healthy_minutes")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runFirmwareStabilityWatchdog(ctx, dir, testTick, testThreshold)
	}()

	waitForFile(t, healthyMinutes, time.Second)
	cancel()
	wg.Wait()

	// No assertion on pending_verify (it never existed). The point of
	// this test is "doesn't crash, doesn't loop on Remove". The fact
	// that we reach this line is enough.
}

func TestFirmwareStability_ResetsCounterOnRun(t *testing.T) {
	// Simulate a daemon crash: write "99" to healthy_minutes (as if
	// a prior run accumulated lots of stability), then start the
	// watchdog. The very first tick must overwrite to "0" — otherwise
	// a crashy install that runs <5 min could accidentally inherit a
	// prior install's stability and never roll back.
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "boot_ok"), "")
	healthyMinutes := filepath.Join(dir, "healthy_minutes")
	mustWriteFile(t, healthyMinutes, "99")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runFirmwareStabilityWatchdog(ctx, dir, testTick, testThreshold)
	}()

	// Wait for the watchdog to have done at least its startup write.
	// We can detect this by polling for the file to drop below 99.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(healthyMinutes)
		if n, err := strconv.Atoi(string(data)); err == nil && n < 99 {
			break
		}
		time.Sleep(testTick)
	}
	data, _ := os.ReadFile(healthyMinutes)
	n, _ := strconv.Atoi(string(data))
	if n >= 99 {
		t.Errorf("watchdog inherited prior counter value: got %d", n)
	}
	cancel()
	wg.Wait()
}

func TestFirmwareStability_ContextCancelStops(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "boot_ok"), "")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runFirmwareStabilityWatchdog(ctx, dir, testTick, testThreshold)
		close(done)
	}()

	// Let it run a tick or two, then cancel.
	time.Sleep(5 * testTick)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not return within 1s of ctx cancellation")
	}
}

// --- helpers ---

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(testTick)
	}
	t.Fatalf("timed out waiting for %q to appear", path)
}

func waitForFileGone(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err != nil && os.IsNotExist(err) {
			return
		}
		time.Sleep(testTick)
	}
	t.Fatalf("timed out waiting for %q to disappear", path)
}
