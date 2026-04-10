package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/cargocam/ghostcam/server/s3"
	"github.com/go-chi/chi/v5"
)

// PrepareClip handles POST /api/v1/clips/prepare.
// Returns presigned GET URLs for all segments in the requested time range.
func (a *App) PrepareClip(w http.ResponseWriter, r *http.Request) {
	var body apitypes.PrepareClipRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.DeviceID == "" || body.FromMs >= body.ToMs {
		writeError(w, http.StatusBadRequest, "device_id, from_ms, and to_ms required (from < to)")
		return
	}

	if _, ok := a.ownedCamera(w, r, body.DeviceID); !ok {
		return
	}

	if a.S3 == nil {
		writeError(w, http.StatusServiceUnavailable, "S3 not configured")
		return
	}

	ctx := r.Context()
	segments, err := a.DB.ListSegments(ctx, body.DeviceID, body.FromMs, body.ToMs, a.retentionMs())
	if err != nil {
		slog.Error("prepare clip: list segments failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if len(segments) == 0 {
		writeJSON(w, http.StatusOK, apitypes.PrepareClipResponse{Segments: []apitypes.ClipSegment{}, TotalBytes: 0, DurationMs: 0})
		return
	}

	result := make([]apitypes.ClipSegment, 0, len(segments))
	var totalBytes uint64
	for _, seg := range segments {
		url, err := a.S3.PresignGet(ctx, s3.SegmentKey(body.DeviceID, seg.SegmentID))
		if err != nil {
			slog.Warn("prepare clip: presign failed", "segment_id", seg.SegmentID, "error", err)
			continue
		}
		result = append(result, apitypes.ClipSegment{
			ID:        seg.SegmentID,
			URL:       url,
			StartMs:   seg.StartTS,
			EndMs:     seg.EndTS,
			SizeBytes: seg.SizeBytes,
		})
		totalBytes += seg.SizeBytes
	}

	durationMs := segments[len(segments)-1].EndTS - segments[0].StartTS

	writeJSON(w, http.StatusOK, apitypes.PrepareClipResponse{
		Segments:   result,
		TotalBytes: totalBytes,
		DurationMs: durationMs,
	})
}

// ExportTelemetry handles GET /api/v1/telemetry/{deviceID}/export?from=&to=&format=csv|json.
func (a *App) ExportTelemetry(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if _, ok := a.ownedCamera(w, r, deviceID); !ok {
		return
	}

	if a.Redis == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	fromMs := parseQueryUint64(r, "from", 0)
	toMs := parseQueryUint64(r, "to", 0)
	if fromMs == 0 || toMs == 0 || fromMs >= toMs {
		writeError(w, http.StatusBadRequest, "from and to required (from < to)")
		return
	}

	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}

	entries, err := redis.QueryTelemetryRange(r.Context(), a.Redis, deviceID, fromMs, toMs, 10000)
	if err != nil {
		slog.Error("export telemetry failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=telemetry.csv")
		w.Write([]byte("ts,server_ts,cpu,mem,temp,uptime,sig,lat,lon,alt,gps_fix\n"))
		for _, e := range entries {
			w.Write([]byte(csvRow(e)))
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", "attachment; filename=telemetry.json")
		json.NewEncoder(w).Encode(apitypes.TelemetryRangeResponse{Entries: entries})
	}
}

func csvRow(e apitypes.TelemetryEntry) string {
	return csvFmt(e.TS) + "," +
		csvFmt(e.ServerTS) + "," +
		csvOptUint32(e.CPU) + "," +
		csvOptUint32(e.Mem) + "," +
		csvOptUint32(e.Temp) + "," +
		csvOptUint32(e.Uptime) + "," +
		csvOptInt8(e.Sig) + "," +
		csvOptFloat64(e.Lat) + "," +
		csvOptFloat64(e.Lon) + "," +
		csvOptFloat32(e.Alt) + "," +
		csvOptUint8(e.GPSFix) + "\n"
}

func csvFmt(v uint64) string {
	return strconv.FormatUint(v, 10)
}
func csvOptUint32(v *uint32) string {
	if v == nil {
		return ""
	}
	return strconv.FormatUint(uint64(*v), 10)
}
func csvOptUint8(v *uint8) string {
	if v == nil {
		return ""
	}
	return strconv.FormatUint(uint64(*v), 10)
}
func csvOptInt8(v *int8) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(int64(*v), 10)
}
func csvOptFloat64(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}
func csvOptFloat32(v *float32) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(float64(*v), 'f', -1, 32)
}
