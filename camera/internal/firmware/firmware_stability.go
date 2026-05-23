package firmware

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Firmware stability watchdog (#106). Closes a gap in the rollback
// gate: today's check (camera/telemetry_poll.go writes boot_ok after
// first successful telemetry; pi/systemd ExecStartPre rolls back when
// boot_ok is missing) only catches binaries that crash BEFORE talking
// to the server. A binary that writes boot_ok and then crashes 30 s
// later in normal operation slips through.
//
// We treat boot_ok as a *liveness* signal and add a second
// *stability* signal: a healthy_minutes counter incremented every
// minute the daemon survives. Once the counter reaches
// firmwareStabilityMinutes, we delete install_pending_verify — the
// install has earned full trust and a future crash won't be treated
// as a post-install regression.
//
// ExecStartPre (pi/systemd/ghostcam-camera.service) reads both files
// on the next boot when install_pending_verify is still present and
// rolls back if either gate is unmet.
//
// Wider failure modes (binary that runs months and then breaks on a
// specific telemetry payload, broken segments / garbled WHIP, etc.)
// remain out of scope here — they need fleet-wide canary blast-radius
// gating, which is the issue's "Suggested next step" item 2.

const (
	firmwareStabilityMinutes      = 5
	firmwareStabilityTickInterval = time.Minute
)

// RunFirmwareStabilityWatchdog blocks until ctx is cancelled. It
// waits for boot_ok to appear (the camera daemon's "reached the
// server" marker) before it begins counting, so a binary that boots
// but never gets online doesn't accidentally accumulate stability
// minutes. Once ticking, it writes the running minute count to
// healthy_minutes every interval and removes install_pending_verify
// at the threshold.
//
// Safe to call from any goroutine; safe to call before / after the
// telemetry poll has started; safe to run on dev / synthetic builds
// (no install_pending_verify exists, the Remove is a no-op).
func RunFirmwareStabilityWatchdog(ctx context.Context, dataDir string) {
	runFirmwareStabilityWatchdog(ctx, dataDir, firmwareStabilityTickInterval, firmwareStabilityMinutes)
}

func runFirmwareStabilityWatchdog(ctx context.Context, dataDir string, tick time.Duration, threshold int) {
	healthFile := filepath.Join(dataDir, "healthy_minutes")
	pendingVerifyFile := filepath.Join(dataDir, "install_pending_verify")
	bootOkFile := filepath.Join(dataDir, "boot_ok")

	// Block until boot_ok exists. Polling: 5 s is fast enough that the
	// watchdog starts within one cycle of telemetry success, slow
	// enough not to be noise.
	pollInterval := 5 * time.Second
	if tick < pollInterval {
		// Tests pass tiny tick intervals; align the boot_ok poll
		// to the same scale so the test isn't gated on real seconds.
		pollInterval = tick
	}
	for {
		if _, err := os.Stat(bootOkFile); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}

	// Reset the counter at run start. If the daemon dies before the
	// first tick, ExecStartPre reads "0" on the next boot and rolls
	// back. Note this can race with ExecStartPre on a same-second
	// service restart, but ExecStartPre always runs first (systemd
	// guarantees), so the worst case is we momentarily look like
	// "no healthy_minutes" — same as a fresh install.
	_ = os.WriteFile(healthFile, []byte("0"), 0644)

	// Only attempt to clear install_pending_verify if it exists at
	// start. Saves us from logging "removed pending_verify" on every
	// reboot of a long-stable install.
	pendingVerifyExists := fileExists(pendingVerifyFile)

	minutes := 0
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			minutes++
			if err := os.WriteFile(healthFile, []byte(strconv.Itoa(minutes)), 0644); err != nil {
				slog.Warn("firmware stability: write healthy_minutes failed",
					"path", healthFile, "minutes", minutes, "err", err)
			}
			if pendingVerifyExists && minutes >= threshold {
				if err := os.Remove(pendingVerifyFile); err == nil {
					slog.Info("firmware install verified, removed install_pending_verify",
						"minutes", minutes, "threshold", threshold)
					pendingVerifyExists = false
				} else if os.IsNotExist(err) {
					pendingVerifyExists = false
				} else {
					slog.Warn("firmware stability: remove install_pending_verify failed",
						"err", err)
				}
			}
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
