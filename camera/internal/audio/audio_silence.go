package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

// Silent-audio sampler (GH #114-adjacent). #114 surfaced after an
// operator noticed by hand that the Voice HAT was outputting silence
// at the noise floor (mean_volume ≈ -90 dBFS). The right fix is to
// reseat the ribbon — hardware — but a periodic measurement lets us
// catch it without anyone running ffprobe by hand, and serves a wider
// purpose: any mic failure, daughterboard power issue, or
// configuration regression (someone setting NoAudio=true accidentally)
// will surface as an alert in the dashboard rather than "we noticed
// after a customer complained the playback was silent."
//
// Camera-side: run ffmpeg with the volumedetect filter against a
// recent segment file once every audioSampleInterval, parse
// mean_volume from stderr, and expose the latest value via an atomic
// the telemetry tick reads. Server-side edge detection lives in
// server/audio_silence.go.

// audioSampleInterval is how often the sampler runs. 5 min trades
// detection latency for CPU load — a Pi Zero 2W can run ffmpeg
// volumedetect against a single 4 s segment in ~250 ms.
const audioSampleInterval = 5 * time.Minute

// segmentMinAgeForSilenceMeasure is how old a .ts file must be before
// we'll read it. ffmpeg's MPEG-TS segmenter rotates every ~4 s; the
// 2-newest pattern alone would race against truncate-then-rewrite on
// some kernels, so we additionally pin to "second-newest AND > 10 s
// old."
const segmentMinAgeForSilenceMeasure = 10 * time.Second

// audioRMSDBFS holds the most recent measurement so ReadTelemetry can
// surface it. nil = sampling disabled / no successful measurement
// yet; otherwise a *float32 with the dBFS value.
var audioRMSDBFS atomic.Pointer[float32]

// ReadAudioRMSDBFS returns the most recent dBFS measurement, or nil
// if none yet. Telemetry uses this directly — the pointer is
// shared, so writes from the sampler goroutine are visible without a
// further atomic load.
func ReadAudioRMSDBFS() *float32 {
	return audioRMSDBFS.Load()
}

// SetAudioRMSDBFS is exported so the test harness (and a future
// inline-in-pipeline sampler that bypasses ffmpeg) can publish values
// without going through RunAudioSilenceSampler.
func SetAudioRMSDBFS(dbfs float32) {
	audioRMSDBFS.Store(&dbfs)
}

// ResetAudioRMSDBFSForTest clears the stored value so subsequent
// tests start clean.
func ResetAudioRMSDBFSForTest() {
	audioRMSDBFS.Store(nil)
}

// meanVolumeRE matches `mean_volume: -90.3 dB` lines in ffmpeg's
// volumedetect output. The filter also emits `max_volume`, `histogram_*`,
// and `n_samples` lines; we only care about mean_volume.
var meanVolumeRE = regexp.MustCompile(`mean_volume:\s*(-?\d+(?:\.\d+)?)\s*dB`)

// ParseMeanVolume extracts the mean_volume dBFS from ffmpeg's
// volumedetect stderr output. Returns an error if no line matches.
// Exported for testing.
func ParseMeanVolume(output string) (float32, error) {
	m := meanVolumeRE.FindStringSubmatch(output)
	if m == nil {
		return 0, errors.New("audio_silence: mean_volume not found in ffmpeg output")
	}
	v, err := strconv.ParseFloat(m[1], 32)
	if err != nil {
		return 0, fmt.Errorf("audio_silence: parse mean_volume %q: %w", m[1], err)
	}
	return float32(v), nil
}

// pickSegmentForMeasurement returns the path of the second-most-recent
// .ts file in segDir whose mtime is older than
// segmentMinAgeForSilenceMeasure. Returns "" if no eligible segment
// exists yet (early boot). Filenames are sorted alphabetically;
// ffmpeg's segmenter writes monotonic counters which sort correctly.
func pickSegmentForMeasurement(segDir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(segDir, "*.ts"))
	if err != nil {
		return "", err
	}
	if len(matches) < 2 {
		return "", nil
	}
	sort.Strings(matches)
	candidate := matches[len(matches)-2]
	info, err := statSegment(candidate)
	if err != nil {
		return "", err
	}
	if time.Since(info.modTime) < segmentMinAgeForSilenceMeasure {
		return "", nil
	}
	return candidate, nil
}

// MeasureSegmentDBFS runs ffmpeg with the volumedetect filter against
// segmentPath and returns the parsed mean_volume in dBFS. ffmpegPath
// resolves via PATH if empty.
func MeasureSegmentDBFS(ctx context.Context, ffmpegPath, segmentPath string) (float32, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	// `-vn` skips video decode entirely so we don't pay the CPU cost
	// of H.264 parsing for a metric we only care about on the audio
	// track. `-nostats -hide_banner -loglevel info` keeps stderr small;
	// volumedetect's per-window summary lines are emitted at info
	// level when the filter finalises (-f null - ensures it runs to
	// completion).
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-nostats", "-hide_banner", "-loglevel", "info",
		"-vn", "-i", segmentPath,
		"-af", "volumedetect",
		"-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("audio_silence: ffmpeg failed: %w (output: %s)", err, stderr.String())
	}
	return ParseMeanVolume(stderr.String())
}

// RunAudioSilenceSampler runs MeasureSegmentDBFS on the second-newest
// segment in segDir every audioSampleInterval and publishes the
// result via SetAudioRMSDBFS. No-ops when noAudio is true (the
// telemetry field stays nil and the server-side detector is dormant).
// Returns on ctx cancellation.
func RunAudioSilenceSampler(ctx context.Context, segDir string, noAudio bool) {
	if noAudio {
		slog.Info("audio silence sampler: NoAudio set, skipping")
		return
	}
	// First tick: short delay so we don't hammer ffmpeg immediately on
	// boot before the segmenter has produced anything.
	first := 30 * time.Second
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		sampleOnce(ctx, segDir)
		timer.Reset(audioSampleInterval)
	}
}

// sampleOnce is one tick of the sampler. Extracted for tests and to
// keep the loop body small.
func sampleOnce(ctx context.Context, segDir string) {
	path, err := pickSegmentForMeasurement(segDir)
	if err != nil {
		slog.Debug("audio silence: pick segment failed", "err", err)
		return
	}
	if path == "" {
		// No segment yet (first ~10 s of boot) — leave the atomic
		// alone so a brief startup gap doesn't clear a previously
		// good value across a daemon restart.
		return
	}
	measureCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dbfs, err := MeasureSegmentDBFS(measureCtx, "", path)
	if err != nil {
		slog.Debug("audio silence: measure failed", "path", path, "err", err)
		return
	}
	SetAudioRMSDBFS(dbfs)
	slog.Debug("audio silence sampled", "dbfs", dbfs, "segment", filepath.Base(path))
}

type segmentStat struct {
	modTime time.Time
}

func statSegment(path string) (segmentStat, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return segmentStat{}, err
	}
	return segmentStat{modTime: fi.ModTime()}, nil
}
