// Ghostcam camera agent. Packaged as `package main` so there is no wrapper
// under cmd/ — the camera binary builds from this directory directly.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/abr"
	"github.com/cargocam/ghostcam/camera/internal/audio"
	"github.com/cargocam/ghostcam/camera/internal/battery"
	"github.com/cargocam/ghostcam/camera/internal/battery/drivers"
	"github.com/cargocam/ghostcam/camera/internal/bt"
	"github.com/cargocam/ghostcam/camera/internal/capture"
	"github.com/cargocam/ghostcam/camera/internal/firmware"
	"github.com/cargocam/ghostcam/camera/internal/network"
	"github.com/cargocam/ghostcam/camera/internal/power"
	"github.com/cargocam/ghostcam/camera/internal/sensors"
	"github.com/cargocam/ghostcam/camera/internal/state"
	"github.com/cargocam/ghostcam/camera/internal/telemetry"
	"github.com/cargocam/ghostcam/camera/internal/uplink"
	"github.com/cargocam/ghostcam/camera/internal/upload"
)

// newConnectedPublisher builds a WHIP publisher and runs the SDP handshake.
// The 15 s timeout is generous enough for ICE+DTLS over a slow uplink but
// tight enough that a wedged server doesn't stall capture forever.
func newConnectedPublisher(ctx context.Context, whipURL, bearer string, withAudio bool) (*capture.Publisher, error) {
	pub, err := capture.NewPublisher(withAudio)
	if err != nil {
		return nil, err
	}
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := pub.Connect(connectCtx, whipURL, bearer); err != nil {
		_ = pub.Close()
		return nil, err
	}
	return pub, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// Seed the in-process power-mode atomics from disk so the capture
	// loop's top-of-iteration check honors the persisted value at
	// startup (boot in Sleep => never spawn the capture pipeline).
	// Battery rules layer on top of the manual mode on the first
	// telemetry tick once we have a battery_pct sample.
	power.SetManualPowerMode(cfg.PowerMode)
	power.SetPowerMode(cfg.PowerMode)
	// Battery rules: persisted file wins; missing file → ship the
	// off-grid solar defaults; explicit `[]` → no rules (operator
	// cleared via the editor).
	if rules, err := battery.LoadBatteryRules(cfg.DataDir); err != nil {
		slog.Warn("failed to load battery rules, continuing with none", "err", err)
	} else if rules == nil {
		rules = battery.DefaultBatteryRules()
		battery.SetBatteryRules(rules)
		slog.Info("battery rules: applying defaults (no persisted file)", "count", len(rules))
	} else if len(rules) > 0 {
		battery.SetBatteryRules(rules)
		slog.Info("battery rules loaded", "count", len(rules))
	} else {
		slog.Info("battery rules: persisted file is empty — operator-cleared, no rules applied")
	}

	// Seed the segment-dir atomic so ReadTelemetry can sample the
	// disk-side backlog without a parameter (#115 bug 2).
	upload.SetSegmentDirForTelemetry(cfg.SegmentDir)

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

	// Provision the cellular data bearer (SIM7600) when an APN is set.
	// Nothing else in the stack creates a NetworkManager gsm connection, so
	// a SIM whose APN isn't auto-detected enables the modem but never
	// connects — no cellular uplink despite healthy hardware. Runs early
	// and independent of provisioning (a cellular-only camera needs this
	// up to even reach the server), in its own goroutine because
	// `nmcli connection up` can block on a cold modem. No-op when no APN is
	// configured or on synthetic / non-Linux builds.
	go func() {
		if err := network.EnsureCellular(ctx, cfg.CellularAPN, cfg.CellularUser, cfg.CellularPass); err != nil {
			slog.Warn("cellular provisioning failed", "err", err)
		}
	}()

	// Force-cellular watchdog. Enforces the persisted force_cellular
	// deadline (set by the force_cellular command) — takes WiFi down while
	// active and restores it the moment the deadline passes, including
	// across a restart, so a bad cellular link can't strand the camera.
	// Inert until a force is requested; no-op on synthetic / non-Linux.
	go uplink.RunForceCellularWatchdog(ctx, cfg.DataDir)

	// SIGUSR1 → dump goroutine stacks to a file in DataDir. Workaround
	// for cargocam/ghostcam#134: the daemon goes quiet on a hidden
	// deadlock (motion-mode + capture-pipeline restart), but
	// `systemctl is-active` still shows running, and SIGQUIT crashes
	// the process — losing the stacks. SIGUSR1 lets the operator
	// snapshot the runtime state in-situ without killing the daemon:
	//   kill -USR1 $(pidof ghostcam-camera)
	//   cat /var/ghostcam/goroutines-<pid>-<ts>.txt
	// Cheap (~few ms even with hundreds of goroutines), doesn't
	// allocate against the deadlocked goroutines themselves —
	// runtime/pprof reads g0 stacks directly.
	go runStackDumper(cfg.DataDir)

	// Battery HAT registration (#73). Empty BatteryHAT leaves the no-op
	// default reader in place, so telemetry's battery_pct stays nil and
	// battery rules never fire. Driver init failure (HAT not actually
	// wired up) is logged but non-fatal — a missing HAT shouldn't keep
	// a grid-powered camera from coming up.
	switch cfg.BatteryHAT {
	case "":
		// no HAT configured
	case "pisugar3":
		if r, err := drivers.NewPiSugar3Reader(ctx, cfg.BatteryI2CBus); err != nil {
			slog.Warn("battery HAT init failed; battery_pct will stay nil",
				"hat", cfg.BatteryHAT, "bus", cfg.BatteryI2CBus, "err", err)
		} else {
			battery.SetBatteryReader(r)
			slog.Info("battery HAT registered", "hat", cfg.BatteryHAT, "bus", cfg.BatteryI2CBus)
		}
	default:
		slog.Warn("unknown GHOSTCAM_BATTERY_HAT; ignoring", "value", cfg.BatteryHAT)
	}

	deviceSerial := sensors.GetDeviceSerial(cfg.DataDir)
	slog.Info("device identity", "serial", deviceSerial)
	sensors.SetGPSSeed(deviceSerial)

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
		network.WaitForRoute(ctx)
	} else {
		// Not provisioned — try briefly for existing network, then enter provisioning.
		// QR scanning may provide WiFi credentials, so don't block forever.
		if !network.WaitForRouteTimeout(ctx, 10*time.Second) {
			slog.Info("no network after 10s, proceeding to provisioning (QR may provide WiFi)")
		}

		slog.Info("no credentials found, entering provisioning mode")
		creds, err = bt.RunProvisioning(ctx, cfg, deviceSerial, identity, Provision)
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
		network.WaitForRoute(ctx)
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

	// Firmware self-update is driven by the telemetry-response
	// `update_firmware` command (see camera/commands.go and the
	// server's apiCommands path in server/telemetry.go). The eager
	// startup check used to live here, but it raced with the
	// `boot_ok` marker: the marker is only written after the first
	// successful telemetry POST, so an eager exit at boot left
	// ExecStartPre seeing a missing marker and treated the upgrade
	// as a crash-rollback target — the camera ended up flipping
	// between the staged binary and its predecessor forever. The
	// telemetry-driven path runs from a stable state where
	// boot_ok has already been set, so the rollback semantics work
	// as intended.

	segments := make(chan upload.NewSegment, 256)

	whipURL := fmt.Sprintf("%s/api/v1/whip/%s", cfg.ServerURL, creds.DeviceID)
	whipPath := fmt.Sprintf("/api/v1/whip/%s", creds.DeviceID)
	whipAudio := !cfg.NoAudio

	var wg sync.WaitGroup

	// Firmware stability watchdog (#106). Bumps a healthy_minutes
	// counter every minute the daemon survives without crashing and
	// removes the install_pending_verify marker at the threshold.
	// ExecStartPre (pi/systemd/ghostcam-camera.service) reads both
	// files on the next boot when the marker is still present and
	// rolls back the install if either gate is unmet. Cheap to run
	// even on dev / synthetic builds (no marker → Remove is a no-op).
	wg.Add(1)
	go func() {
		defer wg.Done()
		firmware.RunFirmwareStabilityWatchdog(ctx, cfg.DataDir)
	}()

	// Standby-wake watchdog. In Standby mode, the capture loop opens
	// a publisher when StandbyWakeActive() is true and skips it
	// otherwise. This goroutine catches the in-flight case: publisher
	// is currently open, but no fresh WakeLive signal has arrived
	// inside standbyWakeWindow — tear down so the next spawn drops
	// the publisher and the cellular bandwidth that comes with it.
	// Cheap: one Load + compare per 15-second tick.
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(15 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				if power.CurrentPowerMode() != power.PowerModeStandby {
					continue
				}
				if power.StandbyWakeActive() {
					continue
				}
				if state.CurrentPublisher() != nil {
					slog.Info("standby wake expired; dropping publisher")
					state.RequestPipelineRestart()
				}
			}
		}
	}()

	// Adaptive bitrate. Default off; opt in via --abr / GHOSTCAM_ABR=1.
	// Runs alongside the capture loop, watches packet loss on the
	// current publisher, and mutates the active tier — which capture
	// reads on next spawn. Tier shifts flow through requestPipelineRestart,
	// so the same restart machinery the telemetry-poll path uses fires
	// the rpicam-vid respawn.
	if cfg.ABREnabled {
		start := state.ABRTier{Name: cfg.ABRStartTier}
		abrCtl := abr.NewABRController(start)
		wg.Add(1)
		go func() {
			defer wg.Done()
			slog.Info("ABR controller starting", "start_tier", cfg.ABRStartTier)
			abrCtl.Run(ctx)
		}()
	}

	// Capture pipeline with crash recovery. Each iteration spins up a fresh
	// WHIP publisher (pion + tracks + handshake) before invoking the
	// capture pipeline. On ffmpeg/rpicam exit, the publisher is closed and
	// recreated on next loop. Per-restart handshake is acceptable because
	// the steady state runs the pipeline for minutes-to-hours per session.
	wg.Add(1)
	go func() {
		defer wg.Done()
		backoff := time.Second
		const maxBackoff = 30 * time.Second
		const stableDuration = 5 * time.Minute
		const maxCrashesBeforeEscalation = 5

		crashCount := 0

		for {
			// Sleep mode: stop the capture pipeline entirely (#112).
			// Stay alive in the loop and poll the power-mode atomic so
			// the wake command (delivered via the slow-cadence
			// telemetry poll) can resume capture without restarting
			// the daemon. Sleep also stops gpsd queries indirectly —
			// without capture there's no segment work to gate.
			for power.CurrentPowerMode() == power.PowerModeSleep {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}

			// Wait if server is unreachable — no point capturing segments
			// that will be evicted before they can upload.
			for upload.IsServerUnreachable() {
				slog.Debug("capture paused, server unreachable")
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Second):
				}
			}

			start := time.Now()
			// Decide whether to open a WHIP publisher this iteration.
			// Live → always. Standby → only when StandbyWakeActive is
			// true (the server-driven wake window). Sleep was handled
			// above. Standby with no wake means capture runs but no
			// publisher is created — segments still upload to S3 via
			// ffmpeg, the cellular bandwidth that WHIP would consume
			// is what's saved.
			var pub *capture.Publisher
			wantPublisher := power.CurrentPowerMode() != power.PowerModeStandby || power.StandbyWakeActive()
			if wantPublisher {
				// Mint a fresh bearer signature for this WHIP session.
				// The server enforces a 5-minute timestamp skew, so we
				// sign at spawn-time and use the same bearer for the
				// SDP POST.
				bearer := SignWHIPBearer(whipPath, creds.DeviceID, identity.PrivateKey)
				var err error
				pub, err = newConnectedPublisher(ctx, whipURL, bearer, whipAudio)
				if err != nil {
					slog.Error("WHIP publisher connect failed; capture will run without live", "err", err)
					pub = nil
				}
			} else {
				slog.Debug("standby: no active viewer, capture without publisher")
			}
			// Publish to telemetry_poll so it can force-close us when the
			// server reports the WHIP session is missing.
			if pub != nil {
				state.SetCurrentPublisher(pub)
			} else {
				state.SetCurrentPublisher(nil)
			}

			// Clear any stale restart request that fired before we got
			// here, then watch for new requests. The watcher cancels
			// captureCtx, which short-circuits StartCapturePipeline so
			// the outer loop can negotiate a fresh WHIP. Without this,
			// a failed initial WHIP connect leaves pub == nil forever
			// (Publisher.Disconnected can't fire on a publisher that
			// was never created).
			state.ResetPipelineRestart()
			captureCtx, cancelCapture := context.WithCancel(ctx)
			restartWatcher := time.NewTicker(2 * time.Second)
			go func() {
				defer restartWatcher.Stop()
				for {
					select {
					case <-captureCtx.Done():
						return
					case <-restartWatcher.C:
						if state.ConsumePipelineRestart() {
							slog.Warn("pipeline restart requested; cancelling capture")
							cancelCapture()
							return
						}
					}
				}
			}()
			err = capture.StartCapturePipeline(captureCtx, cfg, pub)
			cancelCapture()
			state.SetCurrentPublisher(nil)
			if pub != nil {
				_ = pub.Close()
			}
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
			upload.RunSegmentWatcher(ctx, cfg.SegmentDir, cfg.DataDir, cfg.LocalStorageCapBytes, segments)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			upload.RunUploadLoop(ctx, client, cfg.DataDir, segments)
		}()

		// fMP4 segments require an init.mp4 (codec moov box) in S3 for the
		// HLS player to decode them. ffmpeg writes init.mp4 once at
		// pipeline start; this goroutine watches for it and POSTs to the
		// server's /api/v1/cameras/init endpoint, which uploads to
		// s3://<bucket>/<deviceID>/init.mp4.
		wg.Add(1)
		go func() {
			defer wg.Done()
			upload.RunInitUploader(ctx, cfg.SegmentDir, client)
		}()
	} else {
		slog.Info("recording disabled (recording_mode=never) — skipping segment watcher and upload loop")
	}

	// Persistent gpsd WATCH stream so gpsdQuery returns in <1 ms instead
	// of stalling the telemetry loop for up to 5 s per tick on slow
	// receivers (the SIM7600 NMEA path showed this on 2026-05-13).
	wg.Add(1)
	go func() {
		defer wg.Done()
		sensors.StartGpsdReader(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		telemetry.RunTelemetryPoll(ctx, client, cfg.DataDir)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		audio.RunAudioSilenceSampler(ctx, cfg.SegmentDir, cfg.NoAudio)
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
