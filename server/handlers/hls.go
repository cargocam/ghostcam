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

// GetManifest handles GET /hls/{deviceID}/playlist.m3u8?from=&to=
func (h *Handlers) GetManifest(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	deviceID := chi.URLParam(r, "deviceID")
	ctx := r.Context()

	// Verify ownership
	camera, err := h.DB.GetCamera(ctx, deviceID)
	if err != nil || camera == nil || camera.UserID == nil || *camera.UserID != userID {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	if h.S3 == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	nowMs := uint64(time.Now().UnixMilli())
	_, hasFrom := r.URL.Query()["from"]
	_, hasTo := r.URL.Query()["to"]
	toMs := parseQueryUint64(r, "to", nowMs)
	// Live mode (no params): tight 90s window so hls.js gets a small sliding
	// manifest (~15 segments) instead of hundreds. Seeked playback uses the
	// full requested range.
	defaultFrom := toMs - 30*60*1000
	if !hasFrom && !hasTo {
		defaultFrom = toMs - 90*1000
	}
	fromMs := parseQueryUint64(r, "from", defaultFrom)

	// Validate range: from <= to, max 24 hours
	if fromMs > toMs {
		writeError(w, http.StatusBadRequest, "from must be <= to")
		return
	}
	const maxRangeMs = 24 * 60 * 60 * 1000
	if toMs-fromMs > maxRangeMs {
		writeError(w, http.StatusBadRequest, "time range must not exceed 24 hours")
		return
	}

	segments, err := h.DB.ListSegments(ctx, deviceID, fromMs, toMs)
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

	// MPEG-TS segments don't need an init segment (#EXT-X-MAP).
	// Each .ts segment is self-contained with PAT/PMT headers.

	// Media segments — use relative paths so hls.js fetches through our redirect
	// handler, which re-presigns on the fly. This avoids mid-stream URL expiry.
	for _, seg := range segments {
		durationSecs := float64(seg.EndTS-seg.StartTS) / 1000.0
		dt := epochMsToISO8601(seg.StartTS)
		b.WriteString(fmt.Sprintf("#EXT-X-PROGRAM-DATE-TIME:%s\n", dt))
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", durationSecs))
		b.WriteString(fmt.Sprintf("%s.ts\n", seg.SegmentID))
	}

	// Omit EXT-X-ENDLIST for live streams so hls.js keeps polling.
	// Live = no explicit "to" param and latest segment within 2 minutes.
	lastEnd := segments[len(segments)-1].EndTS
	isLive := !hasTo && (nowMs-lastEnd) < 120_000
	if !isLive {
		b.WriteString("#EXT-X-ENDLIST\n")
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(b.String()))
}

// GetInit handles GET /hls/{deviceID}/init.mp4.
func (h *Handlers) GetInit(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	deviceID := chi.URLParam(r, "deviceID")
	ctx := r.Context()

	camera, err := h.DB.GetCamera(ctx, deviceID)
	if err != nil || camera == nil || camera.UserID == nil || *camera.UserID != userID {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	if h.S3 == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	initKey := s3.InitKey(deviceID)
	url, err := h.S3.PresignGet(ctx, initKey)
	if err != nil {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// GetSegment handles GET /hls/{deviceID}/{segmentID}.ts — re-presigns and redirects to S3.
func (h *Handlers) GetSegment(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	deviceID := chi.URLParam(r, "deviceID")
	segmentID := chi.URLParam(r, "segmentID")
	ctx := r.Context()

	camera, err := h.DB.GetCamera(ctx, deviceID)
	if err != nil || camera == nil || camera.UserID == nil || *camera.UserID != userID {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	if h.S3 == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	s3Key := s3.SegmentKey(deviceID, segmentID)
	url, err := h.S3.PresignGet(ctx, s3Key)
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
	userID := ctxutil.GetUserID(r)
	deviceID := chi.URLParam(r, "deviceID")
	ctx := r.Context()

	camera, err := h.DB.GetCamera(ctx, deviceID)
	if err != nil || camera == nil || camera.UserID == nil || *camera.UserID != userID {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	nowMs := uint64(time.Now().UnixMilli())
	fromMs := nowMs - 24*60*60*1000

	segments, err := h.DB.ListSegments(ctx, deviceID, fromMs, nowMs)
	if err != nil {
		slog.Error("list segments failed", "device_id", deviceID, "error", err)
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
