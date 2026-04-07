package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cargocam/ghostcam/api"
	"github.com/cargocam/ghostcam/camera"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	cfg, err := camera.LoadConfig()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// Ensure directories exist
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

	// Wait for network
	camera.WaitForRoute(ctx)

	// Get device serial
	deviceSerial := camera.GetDeviceSerial(cfg.DataDir)
	slog.Info("device identity", "serial", deviceSerial)
	camera.SetGPSSeed(deviceSerial)

	// Load or obtain credentials
	creds := camera.LoadCredentials(cfg.DataDir)
	if creds == nil {
		slog.Info("no credentials found, entering provisioning mode")
		creds, err = camera.RunProvisioning(ctx, cfg, deviceSerial)
		if err != nil {
			slog.Error("provisioning failed", "err", err)
			os.Exit(1)
		}
		if creds == nil {
			slog.Error("no provision_token available and no credentials found")
			os.Exit(1)
		}
	}

	// Override server URL from credentials if not set by config
	if cfg.ServerURL == "" {
		cfg.ServerURL = creds.ServerURL
	}

	slog.Info("starting camera",
		"device_id", creds.DeviceID,
		"server", cfg.ServerURL,
		"test_source", cfg.TestSource,
	)

	client := camera.NewClient(cfg.ServerURL, creds.APIKey, creds.DeviceID)

	// Check for firmware update before starting capture
	if camera.CheckFirmwareUpdate(ctx, client, cfg.DataDir) {
		os.Exit(0) // systemd restarts; ExecStartPre installs the staged binary
	}

	// Segment channel (watcher -> upload loop)
	segments := make(chan camera.NewSegment, 256)

	var wg sync.WaitGroup

	// Start capture pipeline with crash recovery
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
			for camera.IsServerUnreachable() {
				slog.Debug("capture paused, server unreachable")
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Second):
				}
			}

			start := time.Now()
			err := camera.StartCapturePipeline(ctx, cfg)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				slog.Error("capture pipeline failed", "err", err)
			}

			// Only reset after sustained healthy operation (5 min)
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

	// Start segment watcher
	wg.Add(1)
	go func() {
		defer wg.Done()
		camera.RunSegmentWatcher(ctx, cfg.SegmentDir, cfg.DataDir, cfg.LocalStorageCapBytes, segments)
	}()

	// Start upload loop
	wg.Add(1)
	go func() {
		defer wg.Done()
		camera.RunUploadLoop(ctx, client, cfg.DataDir, segments)
	}()

	// Start telemetry poll loop
	wg.Add(1)
	go func() {
		defer wg.Done()
		runTelemetryPoll(ctx, client, cfg.DataDir)
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	slog.Info("shutting down, waiting for goroutines to drain (15s max)")

	// Wait for goroutines with a 15s timeout
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

func runTelemetryPoll(ctx context.Context, client *camera.Client, dataDir string) {
	const (
		baseInterval = 10 * time.Second
		maxInterval  = 60 * time.Second
	)
	interval := baseInterval
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			telemetry := camera.ReadTelemetry()
			commands, err := client.PostTelemetry(ctx, telemetry)
			if err != nil {
				consecutiveFailures++
				slog.Debug("telemetry POST failed", "err", err, "consecutive_failures", consecutiveFailures)
				// Backoff: 10s -> 30s -> 60s (cap)
				switch {
				case consecutiveFailures >= 3:
					interval = maxInterval
				case consecutiveFailures >= 2:
					interval = 30 * time.Second
				default:
					interval = baseInterval
				}
				continue
			}
			// Reset on success
			if consecutiveFailures > 0 {
				consecutiveFailures = 0
				interval = baseInterval
			}
			for _, cmd := range commands {
				handleCommand(ctx, cmd, dataDir)
			}
		}
	}
}

func handleCommand(ctx context.Context, cmd api.CameraCommand, dataDir string) {
	switch cmd.Type {
	case "reboot":
		slog.Info("reboot command received")
		os.Exit(0)
	case "unregister":
		slog.Info("unregister command received, clearing credentials")
		camera.ClearCredentials(dataDir)
		os.Exit(0) // systemd restarts → re-enters provisioning mode
	case "set_recording_mode":
		slog.Info("recording mode change requested", "mode", cmd.Mode)
		if err := camera.WriteStoredFile(dataDir, "recording_mode", cmd.Mode); err != nil {
			slog.Error("failed to persist recording_mode", "err", err)
			return
		}
		slog.Info("recording mode updated, restarting to apply")
		os.Exit(0) // systemd restarts us
	case "set_resolution":
		slog.Info("resolution change requested", "resolution", cmd.Resolution)
		if err := camera.WriteStoredFile(dataDir, "resolution", cmd.Resolution); err != nil {
			slog.Error("failed to persist resolution", "err", err)
			return
		}
		slog.Info("resolution updated, restarting to apply")
		os.Exit(0) // systemd restarts us with new video profile
	case "network_config":
		slog.Info("network config command", "ssid", cmd.SSID)
		go func() {
			psk := cmd.PSK
			if err := camera.EnsureWifi(ctx, cmd.SSID, &psk); err != nil {
				slog.Warn("WiFi config failed", "err", err)
			}
		}()
	case "remove_network":
		slog.Info("remove network command", "ssid", cmd.SSID)
	default:
		slog.Warn("unknown command", "type", cmd.Type)
	}
}
