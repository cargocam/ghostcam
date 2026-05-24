package upload

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/state"
)

// segmentDurationSecs mirrors the target HLS segment length capture
// configures ffmpeg with; the watcher uses it to derive startTS from a
// segment's mtime.
const segmentDurationSecs = state.SegmentDurationSecs

// NewSegment represents a completed .m4s segment file detected by the watcher.
type NewSegment struct {
	Filename   string
	Path       string
	StartTS    uint64 // Unix milliseconds
	EndTS      uint64 // Unix milliseconds
	SizeBytes  uint64
	HasMotion  bool
	RetryCount int // number of upload retries attempted
}

// segmentExt is the file extension produced by ffmpeg's fMP4 HLS muxer. Changing
// this requires matching changes in capture.go's hls_segment_filename pattern,
// the server's SegmentKey, and the HLS manifest's URL emission.
const segmentExt = ".m4s"

// RunSegmentWatcher polls segmentDir every 2 seconds for new .m4s files and
// sends them to the segments channel. It skips 0-byte files and files modified
// less than 2 seconds ago (still being written by ffmpeg).
//
// On startup, only segments tracked in pending_confirms.json are marked as known
// (they've already been uploaded to S3). Any other existing .m4s files are
// treated as new — they'll be re-queued for upload, recovering from crashes.
func RunSegmentWatcher(ctx context.Context, segmentDir, dataDir string, localStorageCap uint64, segments chan<- NewSegment) {
	known := make(map[string]struct{})
	md := newMotionDetector()

	// Seed known from pending confirms — these segments are already uploaded to
	// S3 and waiting for server-side confirmation. All other on-disk .m4s files
	// are orphans that should be re-uploaded.
	if confirms := loadPendingConfirms(dataDir); len(confirms) > 0 {
		for _, c := range confirms {
			known[c.SegmentID+segmentExt] = struct{}{}
		}
		slog.Info("segment watcher: seeded known from pending confirms", "count", len(confirms))
	}

	entries, err := os.ReadDir(segmentDir)
	orphaned := 0
	if err == nil {
		for _, e := range entries {
			if filepath.Ext(e.Name()) == segmentExt {
				if _, ok := known[e.Name()]; !ok {
					orphaned++
				}
			}
		}
	}
	if orphaned > 0 {
		slog.Info("segment watcher: found orphaned segments to re-upload", "count", orphaned)
	}

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

// EnforceLocalStorageCap deletes the oldest media segments in dir until total
// size is under capBytes. init.mp4 (the fMP4 codec init) is never evicted —
// it's tiny and required for any segment to play.
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
		if filepath.Ext(e.Name()) != segmentExt {
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
		if filepath.Ext(name) != segmentExt {
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

		// Validate fMP4 segment header: first top-level box is styp, so
		// bytes 4..8 spell "styp". Skips corrupt/partial files.
		if !isValidFMP4Segment(filepath.Join(dir, name)) {
			slog.Warn("skipping corrupt/partial segment", "file", name)
			continue
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
		hasMotion := md.detect(f.path, f.size)

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

// isValidFMP4Segment checks whether the file starts with an ISO BMFF "styp"
// box. fMP4 media segments emitted by ffmpeg's HLS muxer begin with a Segment
// Type box (styp) at the top level, so bytes 4..8 must spell "styp".
func isValidFMP4Segment(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var buf [8]byte
	if _, err := f.Read(buf[:]); err != nil {
		return false
	}
	return string(buf[4:8]) == "styp"
}
