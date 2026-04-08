package handlers

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/s3"
	"github.com/go-chi/chi/v5"
)

const segmentDurationSecs = 6

// liveWindowMs is the sliding window for live manifests (~15 segments).
const liveWindowMs = 90 * 1000

func (h *Handlers) verifyHLSAccess(r *http.Request) (string, bool) {
	userID := ctxutil.GetUserID(r)
	deviceID := chi.URLParam(r, "deviceID")
	camera, err := h.DB.GetCamera(r.Context(), deviceID)
	if err != nil || camera == nil || camera.UserID == nil || *camera.UserID != userID {
		return "", false
	}
	return deviceID, true
}

// GetLiveManifest handles GET /hls/{deviceID}/live.m3u8
// Returns a small sliding window (~90s) with no EXT-X-ENDLIST so hls.js polls for new segments.
func (h *Handlers) GetLiveManifest(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := h.verifyHLSAccess(r)
	if !ok {
		http.Error(w, "", http.StatusNotFound)
		return
	}
	if h.S3 == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	nowMs := uint64(time.Now().UnixMilli())
	fromMs := nowMs - liveWindowMs

	segments, err := h.DB.ListSegments(r.Context(), deviceID, fromMs, nowMs)
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

	for _, seg := range segments {
		durationSecs := float64(seg.EndTS-seg.StartTS) / 1000.0
		b.WriteString(fmt.Sprintf("#EXT-X-PROGRAM-DATE-TIME:%s\n", epochMsToISO8601(seg.StartTS)))
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", durationSecs))
		b.WriteString(fmt.Sprintf("%s.ts\n", seg.SegmentID))
	}
	// No EXT-X-ENDLIST — hls.js will poll for updates

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(b.String()))
}

// GetVodManifest handles GET /hls/{deviceID}/vod.m3u8?from=&to=
// Returns the full segment range with EXT-X-ENDLIST (finite playlist).
func (h *Handlers) GetVodManifest(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := h.verifyHLSAccess(r)
	if !ok {
		http.Error(w, "", http.StatusNotFound)
		return
	}
	if h.S3 == nil {
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

	segments, err := h.DB.ListSegments(r.Context(), deviceID, fromMs, toMs)
	if err != nil {
		slog.Error("list segments failed", "device_id", deviceID, "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
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

	for _, seg := range segments {
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
func (h *Handlers) GetInit(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := h.verifyHLSAccess(r)
	if !ok {
		http.Error(w, "", http.StatusNotFound)
		return
	}
	if h.S3 == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	initKey := s3.InitKey(deviceID)
	url, err := h.S3.PresignGet(r.Context(), initKey)
	if err != nil {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// GetSegment handles GET /hls/{deviceID}/{segmentID}.ts — re-presigns and redirects to S3.
func (h *Handlers) GetSegment(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := h.verifyHLSAccess(r)
	if !ok {
		http.Error(w, "", http.StatusNotFound)
		return
	}
	if h.S3 == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	segmentID := chi.URLParam(r, "segmentID")
	s3Key := s3.SegmentKey(deviceID, segmentID)
	url, err := h.S3.PresignGet(r.Context(), s3Key)
	if err != nil {
		slog.Warn("presign segment GET failed", "segment_id", segmentID, "error", err)
		http.Error(w, "", http.StatusNotFound)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.Redirect(w, r, url, http.StatusFound)
}

type coverageSegment struct {
	ID        string `json:"id"`
	StartMs   uint64 `json:"start_ms"`
	EndMs     uint64 `json:"end_ms"`
	HasMotion bool   `json:"has_motion"`
}

type coverageResponse struct {
	Segments []coverageSegment `json:"segments"`
}

// GetCoverage handles GET /hls/{deviceID}/coverage.
func (h *Handlers) GetCoverage(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := h.verifyHLSAccess(r)
	if !ok {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	nowMs := uint64(time.Now().UnixMilli())
	// Default to full retention window so all available footage appears on the timeline.
	// Clients can narrow with ?from=&to= if needed.
	retentionMs := uint64(h.RetentionDays) * 24 * 60 * 60 * 1000
	fromMs := parseQueryUint64(r, "from", nowMs-retentionMs)
	toMs := parseQueryUint64(r, "to", nowMs)

	segments, err := h.DB.ListSegmentCoverage(r.Context(), deviceID, fromMs, toMs)
	if err != nil {
		slog.Error("list segment coverage failed", "device_id", deviceID, "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	coverage := make([]coverageSegment, 0, len(segments))
	for _, s := range segments {
		coverage = append(coverage, coverageSegment{
			ID:        s.SegmentID,
			StartMs:   s.StartTS,
			EndMs:     s.EndTS,
			HasMotion: s.HasMotion,
		})
	}

	writeJSON(w, http.StatusOK, coverageResponse{Segments: coverage})
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
