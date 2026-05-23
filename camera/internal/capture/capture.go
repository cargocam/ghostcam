package capture

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/state"
)

const segmentDurationSecs = state.SegmentDurationSecs

// StartCapturePipeline spawns the capture pipeline and blocks until it exits
// or the context is cancelled. For test-source mode it uses ffmpeg testsrc2;
// for real hardware it pipes rpicam-vid into ffmpeg.
//
// pub is the WHIP publisher attached for live streaming; pass nil to disable
// live streaming (the segment pipeline still runs). Raw H.264 from the
// rpicam-vid tee and ffmpeg's OGG/Opus output (fd 3) are bridged to pub via
// the CaptureSink helper, which closes its writer ends on return so the
// publisher's reader goroutines see EOF and exit cleanly.
//
// When cfg.RecordingMode == "never" the MPEG-TS segment sink is omitted but
// the live path still works — capture stays useful for streaming-only.
func StartCapturePipeline(ctx context.Context, cfg *state.CameraConfig, pub *Publisher) error {
	startNum := nextSegmentNumber(cfg.SegmentDir)
	pattern := filepath.Join(cfg.SegmentDir, "seg%05d.m4s")
	kfInterval := fmt.Sprintf("keyint=%d:min-keyint=%d", cfg.VideoKeyframeInterval, cfg.VideoKeyframeInterval)

	// Publish what we're about to spawn so telemetry (#113) surfaces
	// the actually-running parameters — test source and real both
	// share the same publish path. The real pipeline may overwrite
	// with the ABR override; the test pipeline doesn't honour ABR.
	w, h, br := resolveCaptureVideoParams(cfg, state.ActiveTier())
	currentCaptureParams.Store(&captureParams{Width: w, Height: h, Bitrate: br})
	defer currentCaptureParams.Store(nil)

	if cfg.TestSource {
		return runTestPipeline(ctx, cfg, pattern, kfInterval, startNum, pub)
	}
	return runRealPipeline(ctx, cfg, pattern, startNum, pub)
}

// recordsSegments reports whether the pipeline should emit MPEG-TS segments
// for HLS upload. Streaming-only mode suppresses the segment sink.
func recordsSegments(cfg *state.CameraConfig) bool {
	return cfg.RecordingMode != "never"
}

// resolveCaptureVideoParams returns the rpicam-vid resolution and
// bitrate for the next capture spawn. tier, when non-nil, overrides
// the static cfg defaults — this is how the ABR controller (#52) gets
// new tiers in front of rpicam-vid: it writes the active tier, trips
// requestPipelineRestart, and the next spawn reads through this
// function. Pure so tests can pin the override contract without
// running rpicam-vid.
func resolveCaptureVideoParams(cfg *state.CameraConfig, tier *state.ABRTier) (width, height, bitrate uint32) {
	width = cfg.VideoWidth
	height = cfg.VideoHeight
	bitrate = cfg.VideoBitrate
	if tier != nil {
		width = tier.Width
		height = tier.Height
		bitrate = tier.Bitrate
		slog.Info("capture using ABR tier",
			"tier", tier.Name, "width", width, "height", height, "bitrate_kbps", bitrate/1000)
	}
	return
}

// currentCaptureParams is what's actually running RIGHT NOW. Set at
// each runRealPipeline spawn (after resolveCaptureVideoParams) and
// cleared when capture exits, so telemetry (#113) and any future
// consumer always sees the truth, including ABR shifts mid-flight.
var currentCaptureParams atomic.Pointer[captureParams]

// captureParams is the publishable snapshot of resolveCaptureVideoParams.
type captureParams struct {
	Width   uint32
	Height  uint32
	Bitrate uint32 // bps
}

// CurrentCaptureParams returns the rpicam-vid parameters that the
// most-recent runRealPipeline spawned with, or nil when no real
// capture is active (test source, never spawned, sleep mode, between
// restarts).
func CurrentCaptureParams() (width, height, bitrate uint32, ok bool) {
	cp := currentCaptureParams.Load()
	if cp == nil {
		return 0, 0, 0, false
	}
	return cp.Width, cp.Height, cp.Bitrate, true
}

// nextSegmentNumber counts existing .m4s files to avoid filename collisions on restart.
func nextSegmentNumber(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".m4s" {
			count++
		}
	}
	return count
}

func runTestPipeline(ctx context.Context, cfg *state.CameraConfig, pattern, kfInterval string, startNum int, pub *Publisher) error {
	// Prefer pre-encoded test file (no CPU-intensive encoding)
	testFile := filepath.Join(cfg.DataDir, "test-loop.mp4")
	if _, err := os.Stat(testFile); err == nil {
		return runTestFileLoop(ctx, cfg, testFile, pattern, startNum, pub)
	}

	slog.Info("starting test capture pipeline (ffmpeg testsrc2 + sine audio)", "segment_start", startNum)

	size := fmt.Sprintf("%dx%d", cfg.VideoWidth, cfg.VideoHeight)
	videoInput := fmt.Sprintf("testsrc2=size=%s:rate=%d,drawtext=fontfile=/usr/share/fonts/dejavu/DejaVuSansMono.ttf:text='%%{localtime\\:%%T}':fontsize=48:fontcolor=white:x=10:y=10", size, cfg.VideoFPS)
	audioInput := "sine=frequency=440:sample_rate=48000"

	// Create pipe for Opus audio output (fd 3 inside ffmpeg).
	audioPipeR, audioPipeW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating audio pipe: %w", err)
	}
	defer audioPipeR.Close()

	// ffmpeg outputs:
	//   0: fMP4 HLS segments (H.264 + Opus) — for HLS recording (omitted in "never" mode).
	//      Matches the real pipeline's output exactly so the watcher's
	//      isValidFMP4Segment check (styp box at bytes 4..8) accepts them.
	//   1: raw H.264 to stdout — for WebRTC video
	//   2: OGG/Opus to fd 3 — for WebRTC audio
	args := []string{
		"-re",
		"-f", "lavfi", "-i", videoInput,
		"-f", "lavfi", "-i", audioInput,
	}
	if recordsSegments(cfg) {
		// fMP4 segments: init.mp4 (codec moov) + seg*.m4s media fragments.
		// `-hls_list_size 0` keeps the muxer happy; we ignore the
		// playlist file the hls muxer also writes (server builds its own
		// from the segment DB). `temp_file` flag avoids fsync-races
		// against the camera-side watcher.
		playlist := filepath.Join(filepath.Dir(pattern), "playlist.m3u8")
		args = append(args,
			"-map", "0:v", "-map", "1:a",
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-x264-params", kfInterval,
			"-c:a", "libopus", "-b:a", "32k",
			"-application", "voip", "-vbr", "off",
			"-frame_duration", "20",
			"-f", "hls",
			"-hls_segment_type", "fmp4",
			"-hls_fmp4_init_filename", "init.mp4",
			"-hls_segment_filename", pattern,
			"-hls_time", fmt.Sprintf("%d", segmentDurationSecs),
			"-hls_list_size", "0",
			"-hls_flags", "independent_segments+temp_file",
			playlist,
		)
	}
	args = append(args,
		// Output 1: raw H.264 to stdout for WebRTC video
		"-map", "0:v",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-x264-params", kfInterval,
		"-f", "h264",
		"pipe:1",
		// Output 2: OGG/Opus to fd 3 for WebRTC audio
		"-map", "1:a",
		"-c:a", "libopus", "-b:a", "32k",
		"-application", "lowdelay",
		"-frame_duration", "20", "-page_duration", "20000",
		"-f", "ogg",
		"pipe:3",
	)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		audioPipeW.Close()
		return fmt.Errorf("creating stdout pipe: %w", err)
	}

	// Pass the audio pipe write end as fd 3.
	cmd.ExtraFiles = []*os.File{audioPipeW}

	if err := cmd.Start(); err != nil {
		audioPipeW.Close()
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	// Close the write end in the parent — ffmpeg owns it now.
	audioPipeW.Close()
	slog.Info("ffmpeg test pipeline started", "pid", cmd.Process.Pid)

	// Attach the WHIP publisher (no-op if pub is nil).
	sink := NewCaptureSink(ctx, pub)
	defer sink.Close()

	// Copy stdout (raw H.264) to the live publisher.
	go func() {
		_, _ = io.Copy(sink.H264Writer, stdout)
	}()

	// Copy OGG/Opus from ffmpeg fd 3 to the live publisher.
	go func() {
		_, _ = io.Copy(sink.AudioWriter, audioPipeR)
	}()

	err = cmd.Wait()
	if ctx.Err() != nil {
		slog.Info("capture pipeline cancelled")
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("ffmpeg exited: %w", err)
	}
	return nil
}

// runTestFileLoop loops a pre-encoded MP4 file with -c copy (no encoding, minimal CPU).
func runTestFileLoop(ctx context.Context, cfg *state.CameraConfig, testFile, pattern string, startNum int, pub *Publisher) error {
	slog.Info("starting test capture pipeline (pre-encoded loop, -c copy)", "file", testFile, "segment_start", startNum)

	// Create pipe for Opus audio output.
	audioPipeR, audioPipeW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating audio pipe: %w", err)
	}
	defer audioPipeR.Close()

	args := []string{
		"-re",
		"-stream_loop", "-1",
		"-i", testFile,
	}
	if recordsSegments(cfg) {
		// Match the runTestPipeline + production fMP4 output shape so
		// the watcher's isValidFMP4Segment check (styp box at bytes
		// 4..8) accepts them. The previous mpegts-in-.m4s output was
		// silently rejected as "corrupt/partial segment" — same latent
		// bug fixed in 83f08be for the testsrc2 branch, applied here
		// for the pre-encoded-loop branch too. Video copies straight
		// from the loop file; audio re-encodes to Opus (cheap) so the
		// fMP4 muxer gets a codec it understands regardless of what
		// the source MP4 carries.
		playlist := filepath.Join(filepath.Dir(pattern), "playlist.m3u8")
		args = append(args,
			"-map", "0:v", "-map", "0:a",
			"-c:v", "copy",
			"-c:a", "libopus", "-b:a", "32k",
			"-application", "voip", "-vbr", "off",
			"-frame_duration", "20",
			"-f", "hls",
			"-hls_segment_type", "fmp4",
			"-hls_fmp4_init_filename", "init.mp4",
			"-hls_segment_filename", pattern,
			"-hls_time", fmt.Sprintf("%d", segmentDurationSecs),
			"-hls_list_size", "0",
			"-hls_flags", "independent_segments+temp_file",
			playlist,
		)
	}
	args = append(args,
		// Output 1: raw H.264 to stdout
		"-map", "0:v",
		"-c:v", "copy",
		"-f", "h264",
		"pipe:1",
		// Output 2: OGG/Opus to fd 3
		"-map", "0:a",
		"-c:a", "libopus", "-b:a", "32k",
		"-application", "lowdelay",
		"-frame_duration", "20", "-page_duration", "20000",
		"-f", "ogg",
		"pipe:3",
	)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		audioPipeW.Close()
		return fmt.Errorf("creating stdout pipe: %w", err)
	}

	cmd.ExtraFiles = []*os.File{audioPipeW}

	if err := cmd.Start(); err != nil {
		audioPipeW.Close()
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	audioPipeW.Close()
	slog.Info("ffmpeg test file loop started", "pid", cmd.Process.Pid)

	sink := NewCaptureSink(ctx, pub)
	defer sink.Close()

	go func() { _, _ = io.Copy(sink.H264Writer, stdout) }()
	go func() { _, _ = io.Copy(sink.AudioWriter, audioPipeR) }()

	err = cmd.Wait()
	if ctx.Err() != nil {
		slog.Info("capture pipeline cancelled")
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("ffmpeg exited: %w", err)
	}
	return nil
}

func runRealPipeline(ctx context.Context, cfg *state.CameraConfig, pattern string, startNum int, pub *Publisher) error {
	hasAudio := !cfg.NoAudio
	record := recordsSegments(cfg)

	// ABR override: when the controller has picked a tier, run rpicam-vid
	// at the tier's resolution + bitrate instead of the static cfg values.
	// Resolution changes invalidate the fMP4 init segment, which is fine
	// here because the pipeline tears down and respawns on every tier
	// shift (requestPipelineRestart fires from abr.go::shift).
	// StartCapturePipeline already seeded currentCaptureParams; we just
	// read the values back here so the rpicam-vid command matches what
	// telemetry will report.
	width, height, bitrate := resolveCaptureVideoParams(cfg, state.ActiveTier())

	slog.Info("starting real capture pipeline",
		"segment_start", startNum, "records_segments", record, "has_audio", hasAudio,
		"width", width, "height", height, "bitrate_kbps", bitrate/1000)

	// --- rpicam-vid: produces raw H.264 Annex B on stdout ---
	rpicamCmd := exec.CommandContext(ctx, "rpicam-vid",
		"--codec", "h264",
		"--inline",
		"--width", fmt.Sprintf("%d", width),
		"--height", fmt.Sprintf("%d", height),
		"--framerate", fmt.Sprintf("%d", cfg.VideoFPS),
		"--bitrate", fmt.Sprintf("%d", bitrate),
		"-t", "0",
		"-o", "-",
	)
	rpicamCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	rpicamStdout, err := rpicamCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("rpicam stdout pipe: %w", err)
	}

	// Streaming-only with no audio: ffmpeg has nothing to do. Pipe
	// rpicam-vid straight into the live writer and return.
	if !record && !hasAudio {
		return runLiveOnlyRealPipeline(ctx, rpicamCmd, rpicamStdout, pub)
	}

	// Streaming-only short-circuited above, so record is true here and
	// video always pipes through ffmpeg for segment muxing.
	feedVideoToFFmpeg := record

	// --- audio capture: arecord -> ffmpeg via pipe fd ---
	// Background: ffmpeg's libavdevice ALSA reader saturates a single
	// zero2w core on Google VoiceHAT-style I2S MEMS mics (dmesg shows
	// soc_component_trigger -12 errors; arecord at the same config sits at
	// 0-6% CPU). The legacy Python pipeline (commit 18d4d10) hit the same
	// wall and solved it by spawning arecord as a subprocess and feeding
	// raw PCM into ffmpeg via an inherited pipe fd. We do the same: every
	// argument here is copied verbatim from that proven config, including
	// the tight 5 ms period / 20 ms buffer that pulls 1-2 s of latency
	// out of the default arecord buffering.
	const (
		audioSampleRate = 48000
		audioChannels   = 2
	)
	var arecordCmd *exec.Cmd
	var arecordWriteFD *os.File // child's stdout; closed in parent after start
	var arecordReadFD *os.File  // ffmpeg's input; closed in parent after start
	if hasAudio {
		audioDevice := "plughw:0,0"
		if cfg.AudioDevice != "" {
			audioDevice = cfg.AudioDevice
		}
		arecordReadFD, arecordWriteFD, err = os.Pipe()
		if err != nil {
			return fmt.Errorf("creating arecord pipe: %w", err)
		}
		arecordCmd = exec.CommandContext(ctx, "arecord",
			"-q",
			"-D", audioDevice,
			"-f", "S16_LE",
			"-c", fmt.Sprintf("%d", audioChannels),
			"-r", fmt.Sprintf("%d", audioSampleRate),
			"-t", "raw",
			"--period-size", "240",
			"--buffer-size", "960",
		)
		arecordCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		arecordCmd.Stdout = arecordWriteFD
	}

	// --- ffmpeg: muxes fMP4 HLS segments + Opus audio for WHIP ---
	// Low-latency input flags: defaults wait up to 5 s / 5 MB to autodetect
	// stream format, which is dead weight for our declared raw inputs.
	// nobuffer + low_delay turn off ffmpeg's other latency-tolerant defaults.
	ffmpegArgs := []string{
		"-nostdin", "-loglevel", "warning",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-analyzeduration", "0",
		"-probesize", "32",
	}

	if feedVideoToFFmpeg {
		ffmpegArgs = append(ffmpegArgs,
			"-f", "h264", "-framerate", fmt.Sprintf("%d", cfg.VideoFPS),
			"-i", "pipe:0",
		)
	}

	// Audio input via inherited pipe fd. ExtraFiles[0] -> fd 3 in the
	// ffmpeg child; we point its "-i pipe:3" at the arecord-write side of
	// the os.Pipe above. ffmpeg never touches ALSA directly.
	var audioPipeR, audioPipeW *os.File
	if hasAudio {
		ffmpegArgs = append(ffmpegArgs,
			"-f", "s16le",
			"-ar", fmt.Sprintf("%d", audioSampleRate),
			"-ac", fmt.Sprintf("%d", audioChannels),
			"-i", "pipe:3",
		)
	}

	// Optional per-output audio filter chain. I2S MEMS mics (INMP441,
	// etc.) come out 20-30 dB below line level AND have a noticeable
	// noise floor; raw `volume=NdB` amplifies both the speech and the
	// hiss. The chain below applies speech-band shaping first (high-pass
	// 100 Hz to cut enclosure rumble, low-pass 7 kHz to cut high-freq
	// hiss outside the voice band) and then the user-configurable gain.
	// Defaults give a clean +18 dB feel without dragging the noise floor
	// into the foreground.
	audioFilter := []string{}
	if hasAudio {
		// Chain:
		//   highpass=100Hz  — chops enclosure / mounting rumble
		//   agate           — gates the noise floor; ratio 10 + attack
		//                     20 ms / release 250 ms means speech opens
		//                     the gate fast and hangs open through
		//                     pauses without flapping. -32 dB threshold
		//                     sits below typical room-speech level
		//                     (~-20 dB) but well above the INMP441's
		//                     mid-band hiss (~-50 to -45 dB).
		//   lowpass=7kHz    — voice-band shaping, kills high-freq hiss
		//                     residue the gate misses
		//   volume          — user-configurable post-shaping gain
		filters := []string{
			"highpass=f=100",
			"agate=threshold=-32dB:ratio=10:attack=20:release=250",
			"lowpass=f=7000",
		}
		if cfg.AudioGainDB != 0 {
			filters = append(filters, fmt.Sprintf("volume=%ddB", cfg.AudioGainDB))
		}
		audioFilter = []string{"-af", strings.Join(filters, ",")}
	}

	if record {
		// fMP4 HLS output: init.mp4 (codec moov) + .m4s media fragments.
		// Replaces the previous mpegts + AAC pipeline so a single Opus
		// audio encode can be shared with the WHIP track (both speak
		// Opus natively in modern browsers via fMP4 HLS + WebRTC). The
		// hls muxer also writes a local m3u8 we ignore — the server
		// generates the served manifest from the segment DB.
		if hasAudio {
			ffmpegArgs = append(ffmpegArgs,
				"-map", "0:v", "-map", "1:a",
				"-c:v", "copy",
			)
			ffmpegArgs = append(ffmpegArgs, audioFilter...)
			ffmpegArgs = append(ffmpegArgs,
				"-c:a", "libopus", "-b:a", "32k",
				"-application", "voip", "-vbr", "off",
				"-frame_duration", "20",
			)
		} else {
			ffmpegArgs = append(ffmpegArgs,
				"-map", "0:v",
				"-c:v", "copy",
			)
		}
		playlist := filepath.Join(filepath.Dir(pattern), "playlist.m3u8")
		ffmpegArgs = append(ffmpegArgs,
			"-f", "hls",
			"-hls_segment_type", "fmp4",
			"-hls_fmp4_init_filename", "init.mp4",
			"-hls_segment_filename", pattern,
			"-hls_time", fmt.Sprintf("%d", segmentDurationSecs),
			// 0 = unbounded list; we don't read this m3u8 anyway (the
			// server builds its own from the segment DB), but hls muxer
			// requires a value.
			"-hls_list_size", "0",
			"-hls_flags", "independent_segments+temp_file",
			playlist,
		)
	}

	// Live Opus to a side pipe (ExtraFiles[1] -> fd 4 in the child).
	// Same encode params as the segment output — keeps live and recorded
	// audio in step. -application voip and -vbr off come from the proven
	// legacy Python config (commit 18d4d10).
	if hasAudio {
		audioPipeR, audioPipeW, err = os.Pipe()
		if err != nil {
			return fmt.Errorf("creating audio pipe: %w", err)
		}
		defer audioPipeR.Close()

		ffmpegArgs = append(ffmpegArgs, "-map", "1:a")
		ffmpegArgs = append(ffmpegArgs, audioFilter...)
		ffmpegArgs = append(ffmpegArgs,
			"-c:a", "libopus", "-b:a", "32k",
			"-application", "voip", "-vbr", "off",
			"-frame_duration", "20", "-page_duration", "20000",
			"-f", "ogg",
			"pipe:4",
		)
	}

	ffmpegCmd := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
	ffmpegCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// ExtraFiles[0] -> fd 3 (arecord PCM IN), [1] -> fd 4 (opus OUT).
	// Closed in the parent after fork so EOF propagates correctly.
	if hasAudio {
		ffmpegCmd.ExtraFiles = []*os.File{arecordReadFD, audioPipeW}
	}

	var ffmpegStdin io.WriteCloser
	if feedVideoToFFmpeg {
		ffmpegStdin, err = ffmpegCmd.StdinPipe()
		if err != nil {
			if audioPipeW != nil {
				audioPipeW.Close()
			}
			return fmt.Errorf("ffmpeg stdin pipe: %w", err)
		}
	}

	// Cancel handlers: SIGTERM the process group, then SIGKILL after 5s.
	cmds := []*exec.Cmd{rpicamCmd, ffmpegCmd}
	if arecordCmd != nil {
		cmds = append(cmds, arecordCmd)
	}
	for _, cmd := range cmds {
		c := cmd
		c.Cancel = func() error {
			pgid := -c.Process.Pid
			if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
				return err
			}
			go func() {
				time.Sleep(5 * time.Second)
				_ = syscall.Kill(pgid, syscall.SIGKILL)
			}()
			return nil
		}
	}

	// Start arecord first (if needed) so its stdout fd is producing
	// before ffmpeg starts reading. arecord -> arecordWriteFD -> kernel
	// pipe -> arecordReadFD (inherited by ffmpeg as fd 3 via ExtraFiles).
	if arecordCmd != nil {
		if err := arecordCmd.Start(); err != nil {
			arecordReadFD.Close()
			arecordWriteFD.Close()
			if audioPipeR != nil {
				audioPipeR.Close()
				audioPipeW.Close()
			}
			return fmt.Errorf("starting arecord: %w", err)
		}
		// Close the write end in the parent: arecord owns it now, and
		// closing here lets ffmpeg's reader see EOF on arecord exit.
		_ = arecordWriteFD.Close()
		slog.Info("arecord started", "pid", arecordCmd.Process.Pid)
	}

	if err := rpicamCmd.Start(); err != nil {
		if arecordCmd != nil {
			arecordCmd.Process.Kill()
			arecordReadFD.Close()
		}
		if audioPipeW != nil {
			audioPipeW.Close()
		}
		return fmt.Errorf("starting rpicam-vid: %w", err)
	}
	slog.Info("rpicam-vid started", "pid", rpicamCmd.Process.Pid)

	if err := ffmpegCmd.Start(); err != nil {
		rpicamCmd.Process.Kill()
		if arecordCmd != nil {
			arecordCmd.Process.Kill()
		}
		if arecordReadFD != nil {
			arecordReadFD.Close()
		}
		if audioPipeW != nil {
			audioPipeW.Close()
		}
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	// Close child-owned ends in the parent so EOF propagates.
	if arecordReadFD != nil {
		_ = arecordReadFD.Close()
	}
	if audioPipeW != nil {
		_ = audioPipeW.Close()
	}
	slog.Info("ffmpeg started", "pid", ffmpegCmd.Process.Pid)

	// Tear down the pipeline if the WHIP publisher's pc transitions to
	// Failed/Closed (e.g. server restart, ICE timeout). Otherwise ffmpeg
	// keeps pumping bytes into a dead pc forever and the outer reconnect
	// loop in main.go never gets a chance to spin up a fresh WHIP session.
	pipelineCtx, cancelPipeline := context.WithCancel(ctx)
	defer cancelPipeline()
	if pub != nil {
		go func() {
			select {
			case <-pipelineCtx.Done():
			case <-pub.Disconnected():
				slog.Warn("WHIP publisher disconnected; tearing down capture to force reconnect")
				cancelPipeline()
			}
		}()
	}
	// Re-thread the cancel: rpicam/ffmpeg/arecord all started with the
	// parent ctx, but pipelineCtx wraps it. Cancelling pipelineCtx alone
	// won't kill the subprocesses — those need an explicit SIGTERM. Do
	// that here so the disconnect path tears down cleanly.
	go func() {
		<-pipelineCtx.Done()
		if pipelineCtx.Err() != nil && ctx.Err() == nil {
			// Disconnect-driven, not parent-cancelled. Kill subprocesses.
			for _, c := range cmds {
				if c.Process != nil {
					_ = syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
				}
			}
		}
	}()

	// Attach the WHIP publisher (no-op if pub is nil).
	sink := NewCaptureSink(ctx, pub)
	defer sink.Close()

	// Tee rpicam-vid's stdout to ffmpeg's stdin (when recording) and to
	// the live publisher. In streaming-only mode ffmpegStdin is nil so the
	// publisher is the only sink for the H.264 stream.
	teeDone := make(chan error, 1)
	go func() {
		var target io.Writer = sink.H264Writer
		if ffmpegStdin != nil {
			target = io.MultiWriter(ffmpegStdin, sink.H264Writer)
		}
		_, err := io.Copy(target, rpicamStdout)
		if ffmpegStdin != nil {
			ffmpegStdin.Close()
		}
		teeDone <- err
	}()

	// Copy ffmpeg's OGG/Opus output (fd 4 inside the child) to the publisher.
	if hasAudio && audioPipeR != nil {
		go func() { _, _ = io.Copy(sink.AudioWriter, audioPipeR) }()
	}

	rpicamErr := rpicamCmd.Wait()
	ffmpegErr := ffmpegCmd.Wait()
	var arecordErr error
	if arecordCmd != nil {
		arecordErr = arecordCmd.Wait()
	}
	<-teeDone

	if ctx.Err() != nil {
		slog.Info("capture pipeline cancelled")
		return ctx.Err()
	}
	if rpicamErr != nil {
		return fmt.Errorf("rpicam-vid exited: %w", rpicamErr)
	}
	if ffmpegErr != nil {
		return fmt.Errorf("ffmpeg exited: %w", ffmpegErr)
	}
	if arecordErr != nil {
		return fmt.Errorf("arecord exited: %w", arecordErr)
	}
	return nil
}

// runLiveOnlyRealPipeline runs rpicam-vid alone and tees its raw H.264 to the
// live writer. Used when RecordingMode == "never" and audio is disabled, since
// ffmpeg would otherwise have no outputs to produce.
func runLiveOnlyRealPipeline(ctx context.Context, rpicamCmd *exec.Cmd, rpicamStdout io.ReadCloser, pub *Publisher) error {
	rpicamCmd.Cancel = func() error {
		pgid := -rpicamCmd.Process.Pid
		if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
			return err
		}
		go func() {
			time.Sleep(5 * time.Second)
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		}()
		return nil
	}

	if err := rpicamCmd.Start(); err != nil {
		return fmt.Errorf("starting rpicam-vid: %w", err)
	}
	slog.Info("rpicam-vid started (live-only, no recording, no audio)", "pid", rpicamCmd.Process.Pid)

	sink := NewCaptureSink(ctx, pub)
	defer sink.Close()

	teeDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(sink.H264Writer, rpicamStdout)
		teeDone <- err
	}()

	rpicamErr := rpicamCmd.Wait()
	<-teeDone

	if ctx.Err() != nil {
		slog.Info("capture pipeline cancelled")
		return ctx.Err()
	}
	if rpicamErr != nil {
		return fmt.Errorf("rpicam-vid exited: %w", rpicamErr)
	}
	return nil
}
