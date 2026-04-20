package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const segmentDurationSecs = 6

// StartCapturePipeline spawns the capture pipeline and blocks until it exits or
// the context is cancelled. For test-source mode it uses ffmpeg testsrc2; for
// real hardware it pipes rpicam-vid into ffmpeg.
//
// The liveWriter receives a copy of the raw H.264 bytestream for WebRTC live
// relay. Audio Opus packets are pushed via liveWriter.PushAudio from the OGG
// reader goroutine. Pass NullLiveRelay{} to disable live streaming.
//
// When cfg.RecordingMode == "never" the pipeline still runs — the live relay
// output (raw H.264 on stdout + OGG/Opus on fd 3) keeps working — but the
// MPEG-TS segment sink is omitted so nothing is ever written to SegmentDir.
func StartCapturePipeline(ctx context.Context, cfg *CameraConfig, liveWriter LiveWriter) error {
	startNum := nextSegmentNumber(cfg.SegmentDir)
	pattern := filepath.Join(cfg.SegmentDir, "seg%05d.ts")
	kfInterval := fmt.Sprintf("keyint=%d:min-keyint=%d", cfg.VideoKeyframeInterval, cfg.VideoKeyframeInterval)

	if cfg.TestSource {
		return runTestPipeline(ctx, cfg, pattern, kfInterval, startNum, liveWriter)
	}
	return runRealPipeline(ctx, cfg, pattern, startNum, liveWriter)
}

// recordsSegments reports whether the pipeline should emit MPEG-TS segments
// for HLS upload. Streaming-only mode suppresses the segment sink.
func recordsSegments(cfg *CameraConfig) bool {
	return cfg.RecordingMode != "never"
}

// nextSegmentNumber counts existing .ts files to avoid filename collisions on restart.
func nextSegmentNumber(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".ts" {
			count++
		}
	}
	return count
}

func runTestPipeline(ctx context.Context, cfg *CameraConfig, pattern, kfInterval string, startNum int, liveWriter LiveWriter) error {
	// Prefer pre-encoded test file (no CPU-intensive encoding)
	testFile := filepath.Join(cfg.DataDir, "test-loop.mp4")
	if _, err := os.Stat(testFile); err == nil {
		return runTestFileLoop(ctx, cfg, testFile, pattern, startNum, liveWriter)
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
	//   0: MPEG-TS segments (H.264 + AAC) — for HLS recording (omitted in "never" mode)
	//   1: raw H.264 to stdout — for WebRTC video
	//   2: OGG/Opus to fd 3 — for WebRTC audio
	args := []string{
		"-re",
		"-f", "lavfi", "-i", videoInput,
		"-f", "lavfi", "-i", audioInput,
	}
	if recordsSegments(cfg) {
		args = append(args,
			// Output 0: MPEG-TS segments (video + audio)
			"-map", "0:v", "-map", "1:a",
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-x264-params", kfInterval,
			"-c:a", "aac", "-b:a", "64k",
			"-f", "segment",
			"-segment_time", fmt.Sprintf("%d", segmentDurationSecs),
			"-segment_format", "mpegts",
			"-segment_start_number", fmt.Sprintf("%d", startNum),
			"-reset_timestamps", "1",
			pattern,
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
		"-frame_duration", "20",
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

	// Copy stdout (raw H.264) to the live writer.
	go func() {
		io.Copy(liveWriter, stdout)
	}()

	// Read OGG/Opus from the audio pipe and push to the live relay.
	go func() {
		if err := ReadOggOpusPackets(audioPipeR, liveWriter.PushAudio); err != nil {
			slog.Debug("opus reader finished", "err", err)
		}
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
func runTestFileLoop(ctx context.Context, cfg *CameraConfig, testFile, pattern string, startNum int, liveWriter LiveWriter) error {
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
		args = append(args,
			// Output 0: segments
			"-map", "0",
			"-c", "copy",
			"-f", "segment",
			"-segment_time", fmt.Sprintf("%d", segmentDurationSecs),
			"-segment_format", "mpegts",
			"-segment_start_number", fmt.Sprintf("%d", startNum),
			pattern,
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
		"-frame_duration", "20",
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

	go func() {
		io.Copy(liveWriter, stdout)
	}()

	go func() {
		if err := ReadOggOpusPackets(audioPipeR, liveWriter.PushAudio); err != nil {
			slog.Debug("opus reader finished", "err", err)
		}
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

func runRealPipeline(ctx context.Context, cfg *CameraConfig, pattern string, startNum int, liveWriter LiveWriter) error {
	hasAudio := !cfg.NoAudio
	record := recordsSegments(cfg)
	slog.Info("starting real capture pipeline",
		"segment_start", startNum, "records_segments", record, "has_audio", hasAudio)

	// --- rpicam-vid: produces raw H.264 Annex B on stdout ---
	rpicamCmd := exec.CommandContext(ctx, "rpicam-vid",
		"--codec", "h264",
		"--inline",
		"--width", fmt.Sprintf("%d", cfg.VideoWidth),
		"--height", fmt.Sprintf("%d", cfg.VideoHeight),
		"--framerate", fmt.Sprintf("%d", cfg.VideoFPS),
		"--bitrate", fmt.Sprintf("%d", cfg.VideoBitrate),
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
		return runLiveOnlyRealPipeline(ctx, rpicamCmd, rpicamStdout, liveWriter)
	}

	// feedVideoToFFmpeg controls whether H.264 is piped into ffmpeg. We only
	// do so when segments are being recorded. In streaming-only mode with
	// audio we still need ffmpeg for the ALSA→Opus path, but piping H.264
	// into an unmapped input would back-pressure the rpicam-vid tee and
	// stall the live relay — so rpicam-vid's stdout goes straight to
	// liveWriter and ffmpeg only sees the ALSA input.
	feedVideoToFFmpeg := record

	// Stream index of the ALSA input inside ffmpeg. When H.264 is also
	// piped in, ALSA is input 1; otherwise it's the only input (0).
	alsaStreamIdx := 0
	if feedVideoToFFmpeg {
		alsaStreamIdx = 1
	}

	// --- ffmpeg: muxes MPEG-TS segments (when recording) + Opus audio ---
	ffmpegArgs := []string{
		"-nostdin", "-loglevel", "warning",
		"-probesize", "5M", "-analyzeduration", "5M",
	}

	if feedVideoToFFmpeg {
		ffmpegArgs = append(ffmpegArgs,
			"-f", "h264", "-framerate", fmt.Sprintf("%d", cfg.VideoFPS),
			"-i", "pipe:0",
		)
	}

	var audioPipeR, audioPipeW *os.File

	if hasAudio {
		audioDevice := "default"
		if cfg.AudioDevice != "" {
			audioDevice = cfg.AudioDevice
		}
		ffmpegArgs = append(ffmpegArgs, "-f", "alsa", "-i", audioDevice)
	}

	if record {
		if hasAudio {
			ffmpegArgs = append(ffmpegArgs,
				"-map", "0:v", "-map", "1:a",
				"-c:v", "copy", "-c:a", "aac", "-b:a", "64k",
			)
		} else {
			ffmpegArgs = append(ffmpegArgs,
				"-map", "0:v",
				"-c:v", "copy",
			)
		}
		ffmpegArgs = append(ffmpegArgs,
			"-f", "segment",
			"-segment_time", fmt.Sprintf("%d", segmentDurationSecs),
			"-segment_format", "mpegts",
			"-segment_start_number", fmt.Sprintf("%d", startNum),
			"-reset_timestamps", "1",
			pattern,
		)
	}

	// Opus output to fd 3 for WebRTC audio.
	if hasAudio {
		audioPipeR, audioPipeW, err = os.Pipe()
		if err != nil {
			return fmt.Errorf("creating audio pipe: %w", err)
		}
		defer audioPipeR.Close()

		ffmpegArgs = append(ffmpegArgs,
			"-map", fmt.Sprintf("%d:a", alsaStreamIdx),
			"-c:a", "libopus", "-b:a", "32k",
			"-application", "lowdelay",
			"-frame_duration", "20",
			"-f", "ogg",
			"pipe:3",
		)
	}

	ffmpegCmd := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
	ffmpegCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if audioPipeW != nil {
		ffmpegCmd.ExtraFiles = []*os.File{audioPipeW}
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
	for _, cmd := range []*exec.Cmd{rpicamCmd, ffmpegCmd} {
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

	// Start both processes.
	if err := rpicamCmd.Start(); err != nil {
		return fmt.Errorf("starting rpicam-vid: %w", err)
	}
	slog.Info("rpicam-vid started", "pid", rpicamCmd.Process.Pid)

	if err := ffmpegCmd.Start(); err != nil {
		rpicamCmd.Process.Kill()
		if audioPipeW != nil {
			audioPipeW.Close()
		}
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	// Close the write end in the parent — ffmpeg owns it now.
	if audioPipeW != nil {
		audioPipeW.Close()
	}
	slog.Info("ffmpeg started", "pid", ffmpegCmd.Process.Pid)

	// Tee rpicam-vid's stdout to ffmpeg's stdin (when recording) and to
	// the live writer. In streaming-only mode ffmpegStdin is nil so the
	// live writer is the only sink for the H.264 stream.
	teeDone := make(chan error, 1)
	go func() {
		var target io.Writer = liveWriter
		if ffmpegStdin != nil {
			target = io.MultiWriter(ffmpegStdin, liveWriter)
		}
		_, err := io.Copy(target, rpicamStdout)
		if ffmpegStdin != nil {
			ffmpegStdin.Close()
		}
		teeDone <- err
	}()

	// Read Opus audio from the pipe if enabled.
	if hasAudio && audioPipeR != nil {
		go func() {
			if err := ReadOggOpusPackets(audioPipeR, liveWriter.PushAudio); err != nil {
				slog.Debug("opus reader finished", "err", err)
			}
		}()
	}

	rpicamErr := rpicamCmd.Wait()
	ffmpegErr := ffmpegCmd.Wait()
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
	return nil
}

// runLiveOnlyRealPipeline runs rpicam-vid alone and tees its raw H.264 to the
// live writer. Used when RecordingMode == "never" and audio is disabled, since
// ffmpeg would otherwise have no outputs to produce.
func runLiveOnlyRealPipeline(ctx context.Context, rpicamCmd *exec.Cmd, rpicamStdout io.ReadCloser, liveWriter LiveWriter) error {
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

	teeDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(liveWriter, rpicamStdout)
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
