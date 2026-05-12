package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/s3"
	"github.com/go-chi/chi/v5"
)

const segmentDurationSecs = 6

// liveWindowMs is the sliding window size for live manifests. Wide enough
// to absorb a few minutes of upload hiccups (real cameras on cellular
// links go quiet during signal drops, handover, or motion-gated encoding)
// without the manifest endpoint returning 404 and the viewer showing
// "No footage". hls.js only plays the tail of the playlist anyway, so a
// 5-minute window costs almost nothing but buys meaningful resilience.
const liveWindowMs = 5 * 60 * 1000

// retentionMs returns the retention window in milliseconds.
func (a *App) retentionMs() uint64 {
	return uint64(a.Config.retentionDays()) * 24 * 60 * 60 * 1000
}

// GetLiveManifest handles GET /hls/{deviceID}/live.m3u8
// Returns a small sliding window (~90s) with no EXT-X-ENDLIST so hls.js polls for new segments.
func (a *App) GetLiveManifest(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if _, ok := a.ownedCamera(w, r, deviceID); !ok {
		return
	}
	if a.S3 == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	// Owner-check happens above; cache is keyed on deviceID alone
	// because the manifest body is per-device (not per-viewer). 1 s
	// TTL absorbs hls.js's natural ~1 RPS poll cadence per viewer
	// without making the timeline feel stale (new segments land
	// every 6 s anyway). See server/metrics.go.
	hlsCounter.inc()
	if cached := hlsCache.get(deviceID); cached != nil {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(cached)
		return
	}

	nowMs := uint64(time.Now().UnixMilli())
	fromMs := nowMs - liveWindowMs

	segments, err := a.DB.ListSegments(r.Context(), deviceID, fromMs, nowMs, a.retentionMs())
	if err != nil {
		slog.Error("list segments failed", "device_id", deviceID, "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if len(segments) == 0 {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	// Derive MEDIA-SEQUENCE from the first segment's timestamp so it
	// increments as the sliding window advances. hls.js uses this to detect
	// new segments vs stale manifest — a static 0 causes "media sequence
	// mismatch" errors on reload.
	mediaSeq := segments[0].StartTS / (segmentDurationSecs * 1000)

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", segmentDurationSecs))
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	b.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", mediaSeq))

	for i, seg := range segments {
		if i > 0 {
			gap := int64(seg.StartTS) - int64(segments[i-1].EndTS)
			if gap > segmentDurationSecs*2*1000 {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
		}
		durationSecs := float64(seg.EndTS-seg.StartTS) / 1000.0
		b.WriteString(fmt.Sprintf("#EXT-X-PROGRAM-DATE-TIME:%s\n", epochMsToISO8601(seg.StartTS)))
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", durationSecs))
		b.WriteString(fmt.Sprintf("%s.ts\n", seg.SegmentID))
	}
	// No EXT-X-ENDLIST — hls.js will poll for updates.

	body := []byte(b.String())
	hlsCache.put(deviceID, body)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(body)
}

// GetVodManifest handles GET /hls/{deviceID}/vod.m3u8?from=&to=
// Returns the full segment range with EXT-X-ENDLIST (finite playlist).
func (a *App) GetVodManifest(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if _, ok := a.ownedCamera(w, r, deviceID); !ok {
		return
	}
	if a.S3 == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	nowMs := uint64(time.Now().UnixMilli())
	fromMs := parseQueryUint64(r, "from", nowMs-30*60*1000)
	toMs := parseQueryUint64(r, "to", nowMs)

	if fromMs > toMs {
		writeError(w, http.StatusBadRequest, "from must be <= to")
		return
	}
	const maxRangeMs = 24 * 60 * 60 * 1000
	if toMs-fromMs > maxRangeMs {
		writeError(w, http.StatusBadRequest, "time range must not exceed 24 hours")
		return
	}

	segments, err := a.DB.ListSegments(r.Context(), deviceID, fromMs, toMs, a.retentionMs())
	if err != nil {
		slog.Error("list segments failed", "device_id", deviceID, "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Lazy-mode scrub fulfilment: any local-only segments in the
	// requested range get queued onto pending_upload so the next
	// telemetry poll converts them to an `upload_segments` command.
	// Doesn't block this response — the viewer's first scrub will
	// see a gap, the second (after the camera uploads) will see the
	// segments.
	a.markLazyScrubPending(r.Context(), deviceID, fromMs, toMs)

	if len(segments) == 0 {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", segmentDurationSecs))
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")

	for i, seg := range segments {
		if i > 0 {
			gap := int64(seg.StartTS) - int64(segments[i-1].EndTS)
			if gap > segmentDurationSecs*2*1000 {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
		}
		durationSecs := float64(seg.EndTS-seg.StartTS) / 1000.0
		b.WriteString(fmt.Sprintf("#EXT-X-PROGRAM-DATE-TIME:%s\n", epochMsToISO8601(seg.StartTS)))
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", durationSecs))
		b.WriteString(fmt.Sprintf("%s.ts\n", seg.SegmentID))
	}
	b.WriteString("#EXT-X-ENDLIST\n")

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(b.String()))
}

// GetInit handles GET /hls/{deviceID}/init.mp4.
func (a *App) GetInit(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if _, ok := a.ownedCamera(w, r, deviceID); !ok {
		return
	}
	if a.S3 == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	initKey := s3.InitKey(deviceID)
	url, err := a.S3.PresignGet(r.Context(), initKey)
	if err != nil {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// GetSegment handles GET /hls/{deviceID}/{segmentID}.ts — re-presigns and redirects to S3.
func (a *App) GetSegment(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if _, ok := a.ownedCamera(w, r, deviceID); !ok {
		return
	}
	if a.S3 == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	segmentID := chi.URLParam(r, "segmentID")
	s3Key := s3.SegmentKey(deviceID, segmentID)
	url, err := a.S3.PresignGet(r.Context(), s3Key)
	if err != nil {
		slog.Warn("presign segment GET failed", "segment_id", segmentID, "error", err)
		http.Error(w, "", http.StatusNotFound)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.Redirect(w, r, url, http.StatusFound)
}

// GetCoverage handles GET /hls/{deviceID}/coverage.
func (a *App) GetCoverage(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if _, ok := a.ownedCamera(w, r, deviceID); !ok {
		return
	}

	nowMs := uint64(time.Now().UnixMilli())
	// Default to full retention window so all available footage appears on the timeline.
	// Clients can narrow with ?from=&to= if needed.
	retentionMs := a.retentionMs()
	fromMs := parseQueryUint64(r, "from", nowMs-retentionMs)
	toMs := parseQueryUint64(r, "to", nowMs)

	segments, err := a.DB.ListSegmentCoverage(r.Context(), deviceID, fromMs, toMs, retentionMs)
	if err != nil {
		slog.Error("list segment coverage failed", "device_id", deviceID, "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	coverage := make([]apitypes.CoverageSegment, 0, len(segments))
	for _, s := range segments {
		coverage = append(coverage, apitypes.CoverageSegment{
			ID:           s.SegmentID,
			StartMs:      s.StartTS,
			EndMs:        s.EndTS,
			HasMotion:    s.HasMotion,
			UploadedToS3: s.UploadedToS3,
		})
	}

	writeJSON(w, http.StatusOK, apitypes.CoverageResponse{Segments: coverage})
}

// markLazyScrubPending queues every local-only segment in the
// [fromMs, toMs] window for fetch on the camera's next telemetry
// poll. No-op when Redis isn't configured (single-instance dev /
// integration test).
func (a *App) markLazyScrubPending(ctx context.Context, deviceID string, fromMs, toMs uint64) {
	if a.Redis == nil {
		return
	}
	ids, err := a.DB.ListLocalOnlySegmentIDs(ctx, deviceID, fromMs, toMs, 100)
	if err != nil {
		slog.Debug("lazy scrub: list local-only failed", "device_id", deviceID, "error", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	key := pendingUploadKey(deviceID)
	// SAdd is idempotent — concurrent scrubs naturally de-dupe.
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if err := a.Redis.SAdd(ctx, key, args...).Err(); err != nil {
		slog.Debug("lazy scrub: redis SAdd failed", "device_id", deviceID, "error", err)
		return
	}
	// 30 s TTL — well above the telemetry-poll cadence so the next
	// poll picks them up, and bounded so a permanently-offline camera
	// doesn't leak the key.
	a.Redis.Expire(ctx, key, 30*time.Second)
}

func parseQueryUint64(r *http.Request, key string, def uint64) uint64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func epochMsToISO8601(epochMs uint64) string {
	t := time.UnixMilli(int64(epochMs)).UTC()
	return t.Format("2006-01-02T15:04:05.000Z")
}
