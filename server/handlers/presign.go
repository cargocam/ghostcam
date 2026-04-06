package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/api"
	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/s3"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
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

	// 1. Look up camera (needed for storage check and event publishing)
	camera, err := h.DB.GetCamera(ctx, deviceID)
	if err != nil || camera == nil || camera.UserID == nil {
		writeError(w, http.StatusNotFound, "camera not found")
		return
	}
	userID := *camera.UserID

	// 2. Confirm uploaded segments
	var confirmedBytes int64
	if len(body.Uploaded) > 0 {
		records := make([]db.SegmentRecord, 0, len(body.Uploaded))
		now := uint64(time.Now().UnixMilli())
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
			confirmedBytes += int64(u.SizeBytes)
		}
		if err := h.DB.InsertSegments(ctx, records); err != nil {
			slog.Error("presign: insert segments failed", "device_id", deviceID, "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		// Update cached storage counter and publish motion events
		if h.Redis != nil {
			// Increment storage counter with confirmed bytes
			if confirmedBytes > 0 {
				storageKey := "storage_bytes:" + userID
				h.Redis.RDB().IncrBy(ctx, storageKey, confirmedBytes)
				h.Redis.RDB().Expire(ctx, storageKey, 5*time.Minute)
			}

			// Publish motion events to per-user channel
			for _, u := range body.Uploaded {
				if u.HasMotion {
					motionPayload, _ := json.Marshal(map[string]any{
						"device_id":  deviceID,
						"segment_id": u.SegmentID,
						"start_ts":   u.StartTS,
						"end_ts":     u.EndTS,
					})
					if err := h.Redis.RDB().Publish(ctx, fmt.Sprintf("motion:%s", userID), motionPayload).Err(); err != nil {
						slog.Debug("failed to publish motion event", "error", err)
					}
				}
			}
		}
	}

	// 3. Check storage limits
	tierID := defaultTierID
	sub, _ := h.DB.GetSubscription(ctx, userID)
	if sub != nil {
		tierID = sub.Tier
	}
	tier := billing.GetTier(tierID)

	storageLimitBytes := tier.StorageLimitBytes()
	if storageLimitBytes > 0 {
		estimatedBatchBytes := int64(body.Count) * 2 * 1024 * 1024

		currentUsage, err := h.getUserStorageCached(ctx, userID)
		if err != nil {
			slog.Warn("presign: failed to get storage usage", "error", err)
		}

		if currentUsage+uint64(estimatedBatchBytes) >= storageLimitBytes {
			slog.Info("storage capped", "device_id", deviceID, "user_id", userID, "limit", storageLimitBytes)

			// Publish storage_capped event (deduplicated per device, 5 min cooldown)
			if h.Redis != nil {
				dedupeKey := "storage_capped_notified:" + deviceID
				set, _ := h.Redis.RDB().SetNX(ctx, dedupeKey, "1", 5*time.Minute).Result()
				if set {
					capPayload, _ := json.Marshal(map[string]any{
						"user_id":       userID,
						"device_id":     deviceID,
						"storage_bytes": storageLimitBytes,
						"limit_gb":      *tier.StorageLimitGB,
					})
					h.Redis.RDB().Publish(ctx, fmt.Sprintf("storage_capped:%s", userID), capPayload)
				}
			}

			writeJSON(w, http.StatusOK, api.PresignResponse{URLs: nil, StorageCapped: true})
			return
		}
	}

	// 4. Generate presigned PUT URLs
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

	// 5. Check if init segment needs uploading
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

// getUserStorageCached returns the user's total segment storage in bytes.
// Uses a Redis counter (TTL 5 min) to avoid the expensive SUM+JOIN on every call.
// Falls back to DB on Redis miss or unavailability.
func (h *Handlers) getUserStorageCached(ctx context.Context, userID string) (uint64, error) {
	if h.Redis != nil {
		storageKey := "storage_bytes:" + userID
		val, err := h.Redis.RDB().Get(ctx, storageKey).Int64()
		if err == nil && val >= 0 {
			return uint64(val), nil
		}
		if err != goredis.Nil {
			slog.Debug("presign: redis storage cache miss", "error", err)
		}

		// Cache miss — compute from DB and populate Redis
		dbVal, dbErr := h.DB.GetUserStorageBytes(ctx, userID)
		if dbErr != nil {
			return 0, dbErr
		}
		h.Redis.RDB().Set(ctx, storageKey, int64(dbVal), 5*time.Minute)
		return dbVal, nil
	}
	return h.DB.GetUserStorageBytes(ctx, userID)
}
