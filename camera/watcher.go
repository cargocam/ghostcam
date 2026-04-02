package camera

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// NewSegment represents a completed .ts segment file detected by the watcher.
type NewSegment struct {
	Filename   string
	Path       string
	StartTS    uint64 // Unix milliseconds
	EndTS      uint64 // Unix milliseconds
	SizeBytes  uint64
	HasMotion  bool
	RetryCount int // number of upload retries attempted
}

// motionDetector maintains a rolling window of segment sizes for motion detection.
type motionDetector struct {
	sizes     []uint64
	maxWindow int
	threshold float64 // motion if size > avg * threshold
}

func newMotionDetector() *motionDetector {
	return &motionDetector{
		maxWindow: 10,
		threshold: 1.5,
	}
}

func (md *motionDetector) detect(sizeBytes uint64) bool {
	if len(md.sizes) < 3 {
		// Not enough history to judge — don't flag as motion
		md.sizes = append(md.sizes, sizeBytes)
		return false
	}

	var sum uint64
	for _, s := range md.sizes {
		sum += s
	}
	avg := float64(sum) / float64(len(md.sizes))
	hasMotion := float64(sizeBytes) > avg*md.threshold

	md.sizes = append(md.sizes, sizeBytes)
	if len(md.sizes) > md.maxWindow {
		md.sizes = md.sizes[1:]
	}

	return hasMotion
}

// RunSegmentWatcher polls segmentDir every 2 seconds for new .ts files and
// sends them to the segments channel. It skips 0-byte files and files modified
// less than 2 seconds ago (still being written by ffmpeg).
func RunSegmentWatcher(ctx context.Context, segmentDir string, localStorageCap uint64, segments chan<- NewSegment) {
	known := make(map[string]struct{})
	md := newMotionDetector()

	// Seed known files so we don't re-upload old segments on restart
	entries, err := os.ReadDir(segmentDir)
	if err == nil {
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".ts" {
				known[e.Name()] = struct{}{}
			}
		}
	}
	slog.Info("segment watcher started, ignoring existing files", "existing", len(known))

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			EnforceLocalStorageCap(segmentDir, localStorageCap)
			scanSegments(segmentDir, known, md, segments)
		}
	}
}

// EnforceLocalStorageCap deletes the oldest .ts files in dir until total size is under capBytes.
func EnforceLocalStorageCap(dir string, capBytes uint64) {
	if capBytes == 0 {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	type fileEntry struct {
		name string
		size uint64
		mod  time.Time
	}

	var files []fileEntry
	var totalSize uint64
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".ts" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		sz := uint64(info.Size())
		files = append(files, fileEntry{name: e.Name(), size: sz, mod: info.ModTime()})
		totalSize += sz
	}

	if totalSize <= capBytes {
		return
	}

	// Sort oldest first
	sort.Slice(files, func(i, j int) bool {
		return files[i].mod.Before(files[j].mod)
	})

	for _, f := range files {
		if totalSize <= capBytes {
			break
		}
		path := filepath.Join(dir, f.name)
		if err := os.Remove(path); err == nil {
			totalSize -= f.size
			slog.Debug("evicted local segment", "file", f.name, "freed_bytes", f.size)
		}
	}
}

func scanSegments(dir string, known map[string]struct{}, md *motionDetector, out chan<- NewSegment) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("failed to read segment dir", "err", err)
		return
	}

	type candidate struct {
		name    string
		path    string
		mtimeMs uint64
		size    uint64
	}
	var newFiles []candidate

	nowMs := uint64(time.Now().UnixMilli())

	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".ts" {
			continue
		}
		if _, ok := known[name]; ok {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		size := uint64(info.Size())
		if size == 0 {
			continue
		}

		mtimeMs := uint64(info.ModTime().UnixMilli())
		if nowMs-mtimeMs < 2000 {
			continue // still being written
		}

		newFiles = append(newFiles, candidate{
			name:    name,
			path:    filepath.Join(dir, name),
			mtimeMs: mtimeMs,
			size:    size,
		})
	}

	// Sort by filename for chronological order
	sort.Slice(newFiles, func(i, j int) bool {
		return newFiles[i].name < newFiles[j].name
	})

	for _, f := range newFiles {
		known[f.name] = struct{}{}

		startTS := f.mtimeMs - segmentDurationSecs*1000
		endTS := f.mtimeMs
		hasMotion := md.detect(f.size)

		slog.Debug("new segment detected", "file", f.name, "size_bytes", f.size, "has_motion", hasMotion)

		seg := NewSegment{
			Filename:  f.name,
			Path:      f.path,
			StartTS:   startTS,
			EndTS:     endTS,
			SizeBytes: f.size,
			HasMotion: hasMotion,
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case out <- seg:
			timer.Stop()
		case <-timer.C:
			slog.Warn("segment channel full after 5s timeout, dropping segment", "file", f.name)
		}
	}
}
