package camera

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"syscall"
)

const segmentDurationSecs = 6

// StartCapturePipeline spawns the capture pipeline and blocks until it exits or
// the context is cancelled. For test-source mode it uses ffmpeg testsrc2; for
// real hardware it pipes rpicam-vid into ffmpeg.
func StartCapturePipeline(ctx context.Context, cfg *CameraConfig) error {
	pattern := filepath.Join(cfg.SegmentDir, "seg%05d.ts")
	kfInterval := fmt.Sprintf("keyint=%d:min-keyint=%d", cfg.VideoKeyframeInterval, cfg.VideoKeyframeInterval)

	if cfg.TestSource {
		return runTestPipeline(ctx, cfg, pattern, kfInterval)
	}
	return runRealPipeline(ctx, cfg, pattern)
}

func runTestPipeline(ctx context.Context, cfg *CameraConfig, pattern, kfInterval string) error {
	slog.Info("starting test capture pipeline (ffmpeg testsrc2 + sine audio)")

	size := fmt.Sprintf("%dx%d", cfg.VideoWidth, cfg.VideoHeight)
	videoInput := fmt.Sprintf("testsrc2=size=%s:rate=%d", size, cfg.VideoFPS)
	audioInput := "sine=frequency=440:sample_rate=48000"

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-re",
		"-f", "lavfi", "-i", videoInput,
		"-f", "lavfi", "-i", audioInput,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-x264-params", kfInterval,
		"-c:a", "aac", "-b:a", "64k",
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%d", segmentDurationSecs),
		"-segment_format", "mpegts",
		"-reset_timestamps", "1",
		pattern,
	)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	slog.Info("ffmpeg test pipeline started", "pid", cmd.Process.Pid)

	err := cmd.Wait()
	if ctx.Err() != nil {
		slog.Info("capture pipeline cancelled")
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("ffmpeg exited: %w", err)
	}
	return nil
}

func runRealPipeline(ctx context.Context, cfg *CameraConfig, pattern string) error {
	slog.Info("starting real capture pipeline (rpicam-vid + ALSA audio | ffmpeg)")

	// Determine audio device (default to "default" ALSA device)
	audioDevice := "default"
	if cfg.AudioDevice != "" {
		audioDevice = cfg.AudioDevice
	}

	// Pipeline: rpicam-vid for video, ffmpeg captures both video pipe + ALSA audio
	// If audio device isn't available, ffmpeg will log a warning but video still works.
	var pipeline string
	if cfg.NoAudio {
		pipeline = fmt.Sprintf(
			"rpicam-vid --codec h264 --inline --width %d --height %d --framerate %d --bitrate %d -t 0 -o - 2>/dev/null | "+
				"ffmpeg -nostdin -loglevel warning -probesize 5M -analyzeduration 5M -f h264 -framerate %d -i pipe:0 "+
				"-c:v copy -f segment -segment_time %d -segment_format mpegts -reset_timestamps 1 '%s'",
			cfg.VideoWidth, cfg.VideoHeight, cfg.VideoFPS, cfg.VideoBitrate,
			cfg.VideoFPS,
			segmentDurationSecs,
			pattern,
		)
	} else {
		pipeline = fmt.Sprintf(
			"rpicam-vid --codec h264 --inline --width %d --height %d --framerate %d --bitrate %d -t 0 -o - 2>/dev/null | "+
				"ffmpeg -nostdin -loglevel warning -probesize 5M -analyzeduration 5M "+
				"-f h264 -framerate %d -i pipe:0 "+
				"-f alsa -i %s "+
				"-c:v copy -c:a aac -b:a 64k "+
				"-f segment -segment_time %d -segment_format mpegts -reset_timestamps 1 '%s'",
			cfg.VideoWidth, cfg.VideoHeight, cfg.VideoFPS, cfg.VideoBitrate,
			cfg.VideoFPS,
			audioDevice,
			segmentDurationSecs,
			pattern,
		)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", pipeline)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting capture pipeline: %w", err)
	}
	slog.Info("real capture pipeline started", "pid", cmd.Process.Pid)

	err := cmd.Wait()
	if ctx.Err() != nil {
		slog.Info("capture pipeline cancelled")
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("capture pipeline exited: %w", err)
	}
	return nil
}
