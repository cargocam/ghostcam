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
	toMs := parseQueryUint64(r, "to", nowMs)
	fromMs := parseQueryUint64(r, "from", toMs-30*60*1000)

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
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")

	// MPEG-TS segments don't need an init segment (#EXT-X-MAP).
	// Each .ts segment is self-contained with PAT/PMT headers.

	// Media segments
	for _, seg := range segments {
		durationSecs := float64(seg.EndTS-seg.StartTS) / 1000.0
		segURL, err := h.S3.PresignGet(ctx, seg.S3Key)
		if err != nil {
			slog.Warn("presign segment GET failed", "segment_id", seg.SegmentID, "error", err)
			continue
		}
		dt := epochMsToISO8601(seg.StartTS)
		b.WriteString(fmt.Sprintf("#EXT-X-PROGRAM-DATE-TIME:%s\n", dt))
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", durationSecs))
		b.WriteString(segURL)
		b.WriteByte('\n')
	}

	// Omit EXT-X-ENDLIST for live streams so hls.js keeps polling.
	// Live = no explicit "to" param and latest segment within 2 minutes.
	lastEnd := segments[len(segments)-1].EndTS
	_, hasTo := r.URL.Query()["to"]
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

type coverageSegment struct {
	ID      string `json:"id"`
	StartMs uint64 `json:"start_ms"`
	EndMs   uint64 `json:"end_ms"`
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
			ID:      s.SegmentID,
			StartMs: s.StartTS,
			EndMs:   s.EndTS,
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
