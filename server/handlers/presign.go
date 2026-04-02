package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/api"
	"github.com/cargocam/ghostcam/server/billing"
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
				HasMotion:  u.HasMotion,
			})
		}
		if err := h.DB.InsertSegments(ctx, records); err != nil {
			slog.Error("presign: insert segments failed", "device_id", deviceID, "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		// Publish motion events via Redis for SSE
		if h.Redis != nil {
			for _, u := range body.Uploaded {
				if u.HasMotion {
					motionPayload, _ := json.Marshal(map[string]any{
						"device_id":  deviceID,
						"segment_id": u.SegmentID,
						"start_ts":   u.StartTS,
						"end_ts":     u.EndTS,
					})
					if err := h.Redis.RDB().Publish(ctx, "motion_detected", motionPayload).Err(); err != nil {
						slog.Debug("failed to publish motion event", "error", err)
					}
				}
			}
		}
	}

	// 2. Check storage limits
	camera, err := h.DB.GetCamera(ctx, deviceID)
	if err != nil || camera == nil || camera.UserID == nil {
		writeError(w, http.StatusNotFound, "camera not found")
		return
	}

	// Determine user's tier
	tierID := defaultTierID
	sub, _ := h.DB.GetSubscription(ctx, *camera.UserID)
	if sub != nil {
		tierID = sub.Tier
	}
	tier := billing.GetTier(tierID)

	storageLimitBytes := tier.StorageLimitBytes()
	if storageLimitBytes > 0 {
		// Use Redis atomic increment to avoid TOCTOU race when checking storage limits.
		// Reserve estimated storage for the batch, then roll back if over limit.
		estimatedBatchBytes := int64(body.Count) * 2 * 1024 * 1024 // ~2MB per segment estimate
		var overLimit bool

		if h.Redis != nil {
			reserveKey := "storage_reserved:" + *camera.UserID
			newTotal, err := h.Redis.RDB().IncrBy(ctx, reserveKey, estimatedBatchBytes).Result()
			if err != nil {
				slog.Warn("presign: redis INCRBY failed, falling back to DB check", "error", err)
				// Fall back to non-atomic DB check
				currentUsage, err := h.DB.GetUserStorageBytes(ctx, *camera.UserID)
				if err == nil && currentUsage >= storageLimitBytes {
					overLimit = true
				}
			} else {
				// Set TTL on the reservation key so it auto-expires (re-syncs from DB)
				h.Redis.RDB().Expire(ctx, reserveKey, 5*time.Minute)

				// Get actual DB usage + reservation to check limit
				currentUsage, dbErr := h.DB.GetUserStorageBytes(ctx, *camera.UserID)
				if dbErr != nil {
					slog.Warn("presign: failed to get user storage", "error", dbErr)
				} else if currentUsage+uint64(newTotal) >= storageLimitBytes {
					// Over limit — roll back reservation
					h.Redis.RDB().DecrBy(ctx, reserveKey, estimatedBatchBytes)
					overLimit = true
				}
			}
		} else {
			currentUsage, err := h.DB.GetUserStorageBytes(ctx, *camera.UserID)
			if err != nil {
				slog.Warn("presign: failed to get user storage", "error", err)
			} else if currentUsage >= storageLimitBytes {
				overLimit = true
			}
		}

		if overLimit {
			slog.Info("storage capped", "device_id", deviceID, "user_id", *camera.UserID, "limit", storageLimitBytes)

			// Publish storage_capped event (deduplicated per device, 5 min cooldown)
			if h.Redis != nil {
				dedupeKey := "storage_capped_notified:" + deviceID
				set, _ := h.Redis.RDB().SetNX(ctx, dedupeKey, "1", 5*time.Minute).Result()
				if set {
					capPayload, _ := json.Marshal(map[string]any{
						"user_id":       *camera.UserID,
						"device_id":     deviceID,
						"storage_bytes": storageLimitBytes,
						"limit_gb":      *tier.StorageLimitGB,
					})
					h.Redis.RDB().Publish(ctx, "storage_capped", capPayload)
				}
			}

			writeJSON(w, http.StatusOK, api.PresignResponse{URLs: nil, StorageCapped: true})
			return
		}
	}

	// 3. Generate presigned PUT URLs
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
