package upload

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// RunInitUploader watches for the fMP4 init segment that ffmpeg writes once at
// pipeline start (and again whenever the encoder restarts) and uploads it to
// the server at s3://<bucket>/<deviceID>/init.mp4. The HLS manifest's
// #EXT-X-MAP tag points at that key; without a current init in S3 the .m4s
// media segments are unplayable.
//
// We poll the segment directory rather than inotify-watching: capture restarts
// rewrite init.mp4 with potentially different codec params, and the polling
// loop catches that change via mtime. init.mp4 is ~1-2 KB, so re-uploads are
// cheap.
func RunInitUploader(ctx context.Context, segmentDir string, client Client) {
	initPath := filepath.Join(segmentDir, "init.mp4")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastMtime time.Time
	var lastUploaded []byte
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		info, err := os.Stat(initPath)
		if err != nil {
			continue
		}
		if info.Size() == 0 {
			continue
		}
		// Still being written? Wait a tick.
		if time.Since(info.ModTime()) < 2*time.Second {
			continue
		}
		if info.ModTime().Equal(lastMtime) {
			continue
		}

		data, err := os.ReadFile(initPath)
		if err != nil {
			slog.Warn("read init.mp4 failed", "err", err)
			continue
		}
		// mtime changes can fire even when ffmpeg rewrites identical bytes
		// (rounded keyframe restart). Avoid re-uploading the same blob.
		if bytes.Equal(data, lastUploaded) {
			lastMtime = info.ModTime()
			continue
		}

		if err := client.UploadInit(ctx, data); err != nil {
			slog.Warn("upload init.mp4 failed", "err", err)
			continue
		}
		slog.Info("init.mp4 uploaded", "size_bytes", len(data))
		lastMtime = info.ModTime()
		lastUploaded = data
	}
}
