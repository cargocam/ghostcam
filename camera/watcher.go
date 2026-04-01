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
	Filename  string
	Path      string
	StartTS   uint64 // Unix milliseconds
	EndTS     uint64 // Unix milliseconds
	SizeBytes uint64
}

// RunSegmentWatcher polls segmentDir every 2 seconds for new .ts files and
// sends them to the segments channel. It skips 0-byte files and files modified
// less than 2 seconds ago (still being written by ffmpeg).
func RunSegmentWatcher(ctx context.Context, segmentDir string, segments chan<- NewSegment) {
	known := make(map[string]struct{})

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
			scanSegments(segmentDir, known, segments)
		}
	}
}

func scanSegments(dir string, known map[string]struct{}, out chan<- NewSegment) {
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

		slog.Debug("new segment detected", "file", f.name, "size_bytes", f.size)

		select {
		case out <- NewSegment{
			Filename:  f.name,
			Path:      f.path,
			StartTS:   startTS,
			EndTS:     endTS,
			SizeBytes: f.size,
		}:
		default:
			slog.Warn("segment channel full, dropping segment", "file", f.name)
		}
	}
}
