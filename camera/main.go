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

	WaitForRoute(ctx)

	deviceSerial := GetDeviceSerial(cfg.DataDir)
	slog.Info("device identity", "serial", deviceSerial)
	SetGPSSeed(deviceSerial)

	creds := LoadCredentials(cfg.DataDir)
	if creds == nil {
		slog.Info("no credentials found, entering provisioning mode")
		creds, err = RunProvisioning(ctx, cfg, deviceSerial)
		if err != nil {
			slog.Error("provisioning failed", "err", err)
			os.Exit(1)
		}
		if creds == nil {
			slog.Error("no provision_token available and no credentials found")
			os.Exit(1)
		}
	}

	if cfg.ServerURL == "" {
		cfg.ServerURL = creds.ServerURL
	}

	slog.Info("starting camera",
		"device_id", creds.DeviceID,
		"server", cfg.ServerURL,
		"test_source", cfg.TestSource,
	)

	client := NewClient(cfg.ServerURL, creds.APIKey, creds.DeviceID)

	if CheckFirmwareUpdate(ctx, client, cfg.DataDir) {
		os.Exit(0) // systemd restarts; ExecStartPre installs the staged binary
	}

	segments := make(chan NewSegment, 256)

	var wg sync.WaitGroup

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
			err := StartCapturePipeline(ctx, cfg)
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
