package main

import (
	"bufio"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
)

// motionDetector compares average P-frame sizes across segments to detect motion.
// H.264 P-frames encode prediction residuals — when visual content changes (motion),
// the encoder can't reuse references efficiently and produces larger P-frames.
// This is more accurate than raw file size because I-frames and audio are excluded.
//
// Falls back to file-size comparison if ffprobe is unavailable.
type motionDetector struct {
	history       []float64 // rolling window of avg P-frame sizes per segment
	maxWindow     int
	threshold     float64 // motion if current avg > rolling avg * threshold
	ffprobeAvail  *bool   // nil = untested, true/false = cached
}

func newMotionDetector() *motionDetector {
	return &motionDetector{
		maxWindow: 10,
		// 1.8 = current P-frame avg must be 80% larger than the rolling
		// average to count as motion. 1.5 (the previous value) tripped on
		// encoder rate-control jitter and slow auto-exposure drift —
		// generating ghost "motion" events on a static scene. 1.8 still
		// trips on actual motion (typically 2-3x baseline as soon as
		// something moves through the frame) while rejecting most of the
		// codec noise.
		threshold: 1.8,
	}
}

// detect returns true if the segment at path shows motion relative to recent history.
func (md *motionDetector) detect(path string, fileSizeBytes uint64) bool {
	avg := md.avgPFrameSize(path)
	if avg <= 0 {
		// ffprobe unavailable or failed — fall back to file size
		avg = float64(fileSizeBytes)
	}

	if len(md.history) < 3 {
		md.history = append(md.history, avg)
		return false
	}

	var sum float64
	for _, v := range md.history {
		sum += v
	}
	rollingAvg := sum / float64(len(md.history))
	hasMotion := avg > rollingAvg*md.threshold

	md.history = append(md.history, avg)
	if len(md.history) > md.maxWindow {
		md.history = md.history[1:]
	}

	return hasMotion
}

// avgPFrameSize runs ffprobe on a segment and returns the average P-frame packet
// size in bytes. Returns 0 if ffprobe is unavailable or the segment has no P-frames.
func (md *motionDetector) avgPFrameSize(path string) float64 {
	if md.ffprobeAvail != nil && !*md.ffprobeAvail {
		return 0 // known unavailable, skip
	}

	cmd := exec.Command("ffprobe",
		"-v", "quiet",
		"-select_streams", "v",
		"-show_entries", "frame=pict_type,pkt_size",
		"-of", "csv=p=0",
		path,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		md.setAvail(false)
		return 0
	}
	if err := cmd.Start(); err != nil {
		md.setAvail(false)
		slog.Debug("ffprobe not available, using file-size fallback for motion detection")
		return 0
	}
	md.setAvail(true)

	var totalSize uint64
	var count int
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), ",")
		parts := strings.SplitN(line, ",", 2)
		if len(parts) < 2 || parts[1] != "P" {
			continue
		}
		size, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			continue
		}
		totalSize += size
		count++
	}
	cmd.Wait()

	if count == 0 {
		return 0
	}
	return float64(totalSize) / float64(count)
}

func (md *motionDetector) setAvail(avail bool) {
	if md.ffprobeAvail == nil {
		md.ffprobeAvail = &avail
	}
}
