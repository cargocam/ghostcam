// Ghostcam camera agent. Packaged as `package main` so there is no wrapper
// under cmd/ — the camera binary builds from this directory directly.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		slog.Error("failed to create data dir", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.SegmentDir, 0755); err != nil {
		slog.Error("failed to create segment dir", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	deviceSerial := GetDeviceSerial(cfg.DataDir)
	slog.Info("device identity", "serial", deviceSerial)
	SetGPSSeed(deviceSerial)

	// Always load or create the ed25519 identity keypair. This is
	// permanent camera identity (like ~/.ssh/id_ed25519) and survives
	// server switches and credential clears.
	identity, err := LoadOrCreateIdentity(cfg.DataDir)
	if err != nil {
		slog.Error("failed to load/create identity", "err", err)
		os.Exit(1)
	}
	slog.Info("camera identity", "device_id", identity.DeviceID)

	creds := LoadCredentials(cfg.DataDir)

	// Detect server switch: env/config URL differs from stored URL.
	// Triggers re-provisioning with the same keypair + new server.
	if creds != nil && cfg.ServerURL != "" && cfg.ServerURL != creds.ServerURL {
		slog.Info("server URL changed, re-provisioning",
			"old", creds.ServerURL, "new", cfg.ServerURL)
		creds = nil
	}

	if creds != nil {
		// Already provisioned — block until network is up.
		WaitForRoute(ctx)
	} else {
		// Not provisioned — try briefly for existing network, then enter provisioning.
		// QR scanning may provide WiFi credentials, so don't block forever.
		if !WaitForRouteTimeout(ctx, 10*time.Second) {
			slog.Info("no network after 10s, proceeding to provisioning (QR may provide WiFi)")
		}

		slog.Info("no credentials found, entering provisioning mode")
		creds, err = RunProvisioning(ctx, cfg, deviceSerial, identity)
		if err != nil {
			slog.Error("provisioning failed", "err", err)
			os.Exit(1)
		}
		if creds == nil {
			slog.Error("no provision_token available and no credentials found")
			os.Exit(1)
		}

		// Ensure network is up after provisioning (QR+WiFi path calls
		// WaitForRoute internally, but file-based provisioning needs it here).
		WaitForRoute(ctx)
	}

	if cfg.ServerURL == "" {
		cfg.ServerURL = creds.ServerURL
	}

	slog.Info("starting camera",
		"device_id", creds.DeviceID,
		"server", cfg.ServerURL,
		"test_source", cfg.TestSource,
	)

	client := NewClient(cfg.ServerURL, creds.DeviceID, identity)

	if CheckFirmwareUpdate(ctx, client, cfg.DataDir) {
		os.Exit(0) // systemd restarts; ExecStartPre installs the staged binary
	}

	segments := make(chan NewSegment, 256)

	// Live relay: parses H.264 NAL units from the capture pipeline tee
	// and makes them available for the WebSocket sender.
	liveRelay := NewLiveRelay(120) // ~4s at 30fps

	var wg sync.WaitGroup

	// Live WebSocket relay — connects to the server and streams H.264
	// NAL units when a viewer is watching. Purely additive; the camera
	// works fine without it (viewers fall back to HLS).
	wg.Add(1)
	go func() {
		defer wg.Done()
		RunLiveRelay(ctx, client, liveRelay)
	}()

	// Capture pipeline with crash recovery.
	wg.Add(1)
	go func() {
		defer wg.Done()
		backoff := time.Second
		const maxBackoff = 30 * time.Second
		const stableDuration = 5 * time.Minute
		const maxCrashesBeforeEscalation = 5

		crashCount := 0

		for {
			// Wait if server is unreachable — no point capturing segments
			// that will be evicted before they can upload.
			for IsServerUnreachable() {
				slog.Debug("capture paused, server unreachable")
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Second):
				}
			}

			start := time.Now()
			err := StartCapturePipeline(ctx, cfg, liveRelay)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				slog.Error("capture pipeline failed", "err", err)
			}

			if time.Since(start) > stableDuration {
				backoff = time.Second
				crashCount = 0
			} else {
				crashCount++
				if crashCount >= maxCrashesBeforeEscalation {
					slog.Error("capture pipeline unstable",
						"crashes", crashCount, "backoff", maxBackoff)
				}
			}

			slog.Info("restarting capture pipeline", "backoff", backoff, "crashes", crashCount)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}

			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}()

	// Segment watcher + upload loop only run when the camera is configured
	// to produce recordings. In "never" mode the capture pipeline has no
	// segment sink (see capture.go), so there would be nothing on disk to
	// watch and nothing to upload. The live relay and telemetry poll still
	// run so the server can issue a set_recording_mode command that flips
	// this back on with the normal process-restart cycle.
	if cfg.RecordingMode != "never" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RunSegmentWatcher(ctx, cfg.SegmentDir, cfg.DataDir, cfg.LocalStorageCapBytes, segments)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			RunUploadLoop(ctx, client, cfg.DataDir, segments)
		}()
	} else {
		slog.Info("recording disabled (recording_mode=never) — skipping segment watcher and upload loop")
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		RunTelemetryPoll(ctx, client, cfg.DataDir)
	}()

	<-ctx.Done()
	slog.Info("shutting down, waiting for goroutines to drain (15s max)")

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		slog.Info("all goroutines drained")
	case <-time.After(15 * time.Second):
		slog.Warn("shutdown timeout, some goroutines did not drain")
	}
	slog.Info("goodbye")
}
