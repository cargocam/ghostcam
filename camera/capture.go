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
// relay. Pass NullLiveRelay{} to disable live streaming.
func StartCapturePipeline(ctx context.Context, cfg *CameraConfig, liveWriter LiveWriter) error {
	startNum := nextSegmentNumber(cfg.SegmentDir)
	pattern := filepath.Join(cfg.SegmentDir, "seg%05d.ts")
	kfInterval := fmt.Sprintf("keyint=%d:min-keyint=%d", cfg.VideoKeyframeInterval, cfg.VideoKeyframeInterval)

	if cfg.TestSource {
		return runTestPipeline(ctx, cfg, pattern, kfInterval, startNum, liveWriter)
	}
	return runRealPipeline(ctx, cfg, pattern, startNum, liveWriter)
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
		return runTestFileLoop(ctx, testFile, pattern, startNum, liveWriter)
	}

	slog.Info("starting test capture pipeline (ffmpeg testsrc2 + sine audio)", "segment_start", startNum)

	size := fmt.Sprintf("%dx%d", cfg.VideoWidth, cfg.VideoHeight)
	videoInput := fmt.Sprintf("testsrc2=size=%s:rate=%d", size, cfg.VideoFPS)
	audioInput := "sine=frequency=440:sample_rate=48000"

	// ffmpeg writes segments to disk AND raw H.264 to stdout for live relay.
	// The -map flags route video to both outputs and audio only to the segment output.
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-re",
		"-f", "lavfi", "-i", videoInput,
		"-f", "lavfi", "-i", audioInput,
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
		// Output 1: raw H.264 to stdout for live relay
		"-map", "0:v",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-x264-params", kfInterval,
		"-f", "h264",
		"pipe:1",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	slog.Info("ffmpeg test pipeline started", "pid", cmd.Process.Pid)

	// Copy stdout (raw H.264) to the live writer in a goroutine.
	go func() {
		io.Copy(liveWriter, stdout)
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
func runTestFileLoop(ctx context.Context, testFile, pattern string, startNum int, liveWriter LiveWriter) error {
	slog.Info("starting test capture pipeline (pre-encoded loop, -c copy)", "file", testFile, "segment_start", startNum)

	// Two outputs: segments to disk + raw H.264 to stdout for live relay.
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-re",
		"-stream_loop", "-1",
		"-i", testFile,
		// Output 0: segments
		"-map", "0",
		"-c", "copy",
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%d", segmentDurationSecs),
		"-segment_format", "mpegts",
		"-segment_start_number", fmt.Sprintf("%d", startNum),
		pattern,
		// Output 1: raw H.264 to stdout for live relay
		"-map", "0:v",
		"-c:v", "copy",
		"-f", "h264",
		"pipe:1",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	slog.Info("ffmpeg test file loop started", "pid", cmd.Process.Pid)

	go func() {
		io.Copy(liveWriter, stdout)
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
	slog.Info("starting real capture pipeline (rpicam-vid | tee | ffmpeg)", "segment_start", startNum)

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

	// --- ffmpeg: reads H.264 from stdin, muxes to MPEG-TS segments ---
	ffmpegArgs := []string{
		"-nostdin", "-loglevel", "warning",
		"-probesize", "5M", "-analyzeduration", "5M",
		"-f", "h264", "-framerate", fmt.Sprintf("%d", cfg.VideoFPS),
		"-i", "pipe:0",
	}

	if !cfg.NoAudio {
		audioDevice := "default"
		if cfg.AudioDevice != "" {
			audioDevice = cfg.AudioDevice
		}
		ffmpegArgs = append(ffmpegArgs, "-f", "alsa", "-i", audioDevice)
		ffmpegArgs = append(ffmpegArgs,
			"-c:v", "copy", "-c:a", "aac", "-b:a", "64k",
		)
	} else {
		ffmpegArgs = append(ffmpegArgs, "-c:v", "copy")
	}

	ffmpegArgs = append(ffmpegArgs,
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%d", segmentDurationSecs),
		"-segment_format", "mpegts",
		"-segment_start_number", fmt.Sprintf("%d", startNum),
		"-reset_timestamps", "1",
		pattern,
	)

	ffmpegCmd := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
	ffmpegCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	ffmpegStdin, err := ffmpegCmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdin pipe: %w", err)
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
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	slog.Info("ffmpeg started", "pid", ffmpegCmd.Process.Pid)

	// Tee rpicam-vid's stdout to both ffmpeg's stdin and the live writer.
	// When rpicam-vid's stdout closes (exit/cancel), the tee goroutine
	// closes ffmpeg's stdin, causing ffmpeg to flush and exit cleanly.
	teeDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.MultiWriter(ffmpegStdin, liveWriter), rpicamStdout)
		ffmpegStdin.Close()
		teeDone <- err
	}()

	// Wait for both processes. rpicam-vid exiting triggers the tee to
	// close ffmpeg's stdin, so ffmpeg follows shortly.
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
