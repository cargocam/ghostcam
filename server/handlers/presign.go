package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/api"
	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/s3"
	"github.com/google/uuid"
)

const presignBatchMax = 10

// Presign handles POST /api/v1/cameras/{deviceID}/presign.
func (h *Handlers) Presign(w http.ResponseWriter, r *http.Request) {
	deviceID := ctxutil.GetCameraDeviceID(r)

	if h.S3 == nil {
		writeError(w, http.StatusServiceUnavailable, "S3 not configured")
		return
	}

	var body api.PresignRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx := r.Context()

	// 1. Confirm uploaded segments
	if len(body.Uploaded) > 0 {
		records := make([]db.SegmentRecord, 0, len(body.Uploaded))
		now := uint64(time.Now().Unix())
		for _, u := range body.Uploaded {
			records = append(records, db.SegmentRecord{
				SegmentID:  u.SegmentID,
				DeviceID:   deviceID,
				S3Key:      s3.SegmentKey(deviceID, u.SegmentID),
				StartTS:    u.StartTS,
				EndTS:      u.EndTS,
				SizeBytes:  u.SizeBytes,
				Resolution: "",
				CreatedAt:  now,
			})
		}
		if err := h.DB.InsertSegments(ctx, records); err != nil {
			slog.Error("presign: insert segments failed", "device_id", deviceID, "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
	}

	// 2. Generate presigned PUT URLs
	count := body.Count
	if count > presignBatchMax {
		count = presignBatchMax
	}

	now := uint64(time.Now().Unix())
	urls := make([]api.PresignedUrl, 0, count)
	for i := uint32(0); i < count; i++ {
		segmentID := uuid.New().String()
		s3Key := s3.SegmentKey(deviceID, segmentID)
		putURL, err := h.S3.PresignPut(ctx, s3Key)
		if err != nil {
			slog.Error("presign PUT failed", "device_id", deviceID, "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		urls = append(urls, api.PresignedUrl{
			SegmentID: segmentID,
			S3Key:     s3Key,
			PutURL:    putURL,
			ExpiresAt: now + h.PresignTTLSecs,
		})
	}

	// 3. Check if init segment needs uploading
	var initURL *api.PresignedUrl
	latest, _ := h.DB.LatestSegment(ctx, deviceID)
	if latest == nil {
		initKey := s3.InitKey(deviceID)
		if putURL, err := h.S3.PresignPut(ctx, initKey); err == nil {
			initURL = &api.PresignedUrl{
				SegmentID: "init",
				S3Key:     initKey,
				PutURL:    putURL,
				ExpiresAt: now + h.PresignTTLSecs,
			}
		}
	}

	writeJSON(w, http.StatusOK, api.PresignResponse{URLs: urls, InitURL: initURL})
}
