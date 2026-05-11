package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/common"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/s3"
)

// PostLocalManifest handles POST /api/v1/cameras/{deviceID}/local-manifest.
//
// Lazy-mode cameras call this once per finished segment that they're
// holding locally rather than uploading. The server records the
// segments with `uploaded_to_s3 = FALSE`, which means:
//
//   - they appear on the viewer's timeline and coverage bar so the
//     user knows footage exists,
//   - GetCoverage marks them with `uploaded_to_s3=false` so the UI can
//     render them differently (hatched fill, scrub-to-fetch UX), and
//   - the first scrub overlapping such a segment triggers the
//     `upload_segments` command path (see hls.go::markLazyScrubPending
//     and telemetry.go::pendingUploadKey).
//
// Authenticated by the same signed Authorization header as presign /
// telemetry. Returns 200 with an empty body on success.
func (a *App) PostLocalManifest(w http.ResponseWriter, r *http.Request) {
	deviceID := getCameraDeviceID(r)

	var body common.LocalManifestRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.Segments) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Owner sanity check — same shape as the presign path so a
	// rogue/orphaned device can't pad the segments table.
	camera, err := a.DB.GetCamera(r.Context(), deviceID)
	if err != nil || camera == nil || camera.UserID == nil {
		writeError(w, http.StatusNotFound, "camera not found")
		return
	}

	records := make([]db.SegmentRecord, 0, len(body.Segments))
	now := uint64(time.Now().UnixMilli())
	for _, s := range body.Segments {
		records = append(records, db.SegmentRecord{
			SegmentID:  s.SegmentID,
			DeviceID:   deviceID,
			S3Key:      s3.SegmentKey(deviceID, s.SegmentID),
			StartTS:    s.StartTS,
			EndTS:      s.EndTS,
			SizeBytes:  s.SizeBytes,
			Resolution: "",
			CreatedAt:  now,
			HasMotion:  s.HasMotion,
		})
	}

	if err := a.DB.InsertLocalSegments(r.Context(), records); err != nil {
		slog.Error("local-manifest: insert failed",
			"device_id", deviceID, "count", len(records), "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	slog.Debug("local-manifest accepted",
		"device_id", deviceID, "count", len(records))
	w.WriteHeader(http.StatusOK)
}
