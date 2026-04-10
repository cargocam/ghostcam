package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/common"
	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/cargocam/ghostcam/server/s3"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

const presignBatchMax = 10

// pruneBatchSize caps the number of segment rows deleted per presign call.
// Prune is tied to normal upload activity instead of a background loop.
const pruneBatchSize = 100

// defaultTierID is the tier assigned to users without a paid Stripe subscription.
const defaultTierID = "free"

// effectiveTier returns the user's billing tier. Paid tiers require an active
// Stripe subscription — a tier column alone is not enough. When Stripe is not
// configured (dev/local), returns "enterprise" (unlimited) so testing works
// without payment infrastructure.
func effectiveTier(sub *db.SubscriptionRecord, stripeConfigured bool) string {
	if !stripeConfigured {
		return "enterprise" // dev mode: unlimited
	}
	if sub == nil {
		return defaultTierID
	}
	if sub.Tier != defaultTierID && (sub.StripeSubscriptionID == nil || sub.Status != "active") {
		return defaultTierID
	}
	return sub.Tier
}

// Presign handles POST /api/v1/cameras/{deviceID}/presign.
func (a *App) Presign(w http.ResponseWriter, r *http.Request) {
	deviceID := getCameraDeviceID(r)

	if a.S3 == nil {
		writeError(w, http.StatusServiceUnavailable, "S3 not configured")
		return
	}

	var body common.PresignRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx := r.Context()

	// 1. Look up camera (needed for storage check and event publishing).
	camera, err := a.DB.GetCamera(ctx, deviceID)
	if err != nil || camera == nil || camera.UserID == nil {
		writeError(w, http.StatusNotFound, "camera not found")
		return
	}
	userID := *camera.UserID

	// 2. Confirm uploaded segments.
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
		if err := a.DB.InsertSegments(ctx, records); err != nil {
			slog.Error("presign: insert segments failed", "device_id", deviceID, "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		if a.Redis != nil {
			if confirmedBytes > 0 {
				storageKey := "storage_bytes:" + userID
				a.Redis.IncrBy(ctx, storageKey, confirmedBytes)
				a.Redis.Expire(ctx, storageKey, 5*time.Minute)
			}

			covSegments := make([]map[string]any, 0, len(body.Uploaded))
			for _, u := range body.Uploaded {
				covSegments = append(covSegments, map[string]any{
					"id":         u.SegmentID,
					"start_ms":   u.StartTS,
					"end_ms":     u.EndTS,
					"has_motion": u.HasMotion,
				})
				if u.HasMotion {
					motionPayload, _ := json.Marshal(map[string]any{
						"device_id":  deviceID,
						"segment_id": u.SegmentID,
						"start_ts":   u.StartTS,
						"end_ts":     u.EndTS,
					})
					eventID, _ := redis.WriteEvent(ctx, a.Redis, userID, deviceID, "motion_detected", string(motionPayload))
					motionWithID, _ := json.Marshal(map[string]any{
						"event_id":   eventID,
						"device_id":  deviceID,
						"segment_id": u.SegmentID,
						"start_ts":   u.StartTS,
						"end_ts":     u.EndTS,
					})
					if err := a.Redis.Publish(ctx, fmt.Sprintf("motion:%s", userID), motionWithID).Err(); err != nil {
						slog.Debug("failed to publish motion event", "error", err)
					}
				}
			}
			covPayload, _ := json.Marshal(map[string]any{
				"device_id": deviceID,
				"segments":  covSegments,
			})
			a.Redis.Publish(ctx, fmt.Sprintf("coverage:%s", userID), covPayload)
		}

		// Opportunistically prune expired segment rows for this device.
		// Tied to upload activity so no background loop is needed; the S3
		// lifecycle rule handles the corresponding object expiry on the
		// bucket side, so we only need to drop the DB rows here.
		cutoff := uint64(time.Now().Add(-time.Duration(a.Config.retentionDays())*24*time.Hour).UnixMilli())
		prunedBytes, err := a.DB.PruneSegments(ctx, deviceID, cutoff, pruneBatchSize)
		if err != nil {
			slog.Warn("presign: segment prune failed", "device_id", deviceID, "error", err)
		} else if prunedBytes > 0 && a.Redis != nil {
			a.Redis.DecrBy(ctx, "storage_bytes:"+userID, prunedBytes)
		}
	}

	// 3. Check tier limits.
	sub, _ := a.DB.GetSubscription(ctx, userID)
	tier := billing.GetTier(effectiveTier(sub, a.stripeConfigured()))

	// Camera limit: only the N oldest cameras (by enrolled_at) may upload.
	if tier.CameraLimit != nil {
		count, err := a.DB.GetCameraCount(ctx, userID)
		if err == nil && count > int64(*tier.CameraLimit) {
			cameras, err := a.DB.ListCameras(ctx, userID)
			if err == nil {
				allowed := make(map[string]bool, *tier.CameraLimit)
				for i := 0; i < *tier.CameraLimit && i < len(cameras); i++ {
					allowed[cameras[i].DeviceID] = true
				}
				if !allowed[deviceID] {
					slog.Info("camera over tier limit", "device_id", deviceID, "user_id", userID, "limit", *tier.CameraLimit, "count", count)
					writeJSON(w, http.StatusOK, common.PresignResponse{URLs: nil, StorageCapped: true})
					return
				}
			}
		}
	}

	storageLimitBytes := tier.StorageLimitBytes()
	if storageLimitBytes > 0 {
		estimatedBatchBytes := int64(body.Count) * 2 * 1024 * 1024

		currentUsage, err := a.getUserStorageCached(ctx, userID)
		if err != nil {
			slog.Warn("presign: failed to get storage usage", "error", err)
		}

		if currentUsage+uint64(estimatedBatchBytes) >= storageLimitBytes {
			slog.Info("storage capped", "device_id", deviceID, "user_id", userID, "limit", storageLimitBytes)

			if a.Redis != nil {
				dedupeKey := "storage_capped_notified:" + deviceID
				set, _ := a.Redis.SetNX(ctx, dedupeKey, "1", 5*time.Minute).Result()
				if set {
					capPayload, _ := json.Marshal(map[string]any{
						"user_id":       userID,
						"device_id":     deviceID,
						"storage_bytes": storageLimitBytes,
						"limit_gb":      *tier.StorageLimitGB,
					})
					eventID, _ := redis.WriteEvent(ctx, a.Redis, userID, deviceID, "storage_capped", string(capPayload))
					capWithID, _ := json.Marshal(map[string]any{
						"event_id":      eventID,
						"user_id":       userID,
						"device_id":     deviceID,
						"storage_bytes": storageLimitBytes,
						"limit_gb":      *tier.StorageLimitGB,
					})
					a.Redis.Publish(ctx, fmt.Sprintf("storage_capped:%s", userID), capWithID)
				}
			}

			writeJSON(w, http.StatusOK, common.PresignResponse{URLs: nil, StorageCapped: true})
			return
		}
	}

	// 4. Generate presigned PUT URLs.
	count := body.Count
	if count > presignBatchMax {
		count = presignBatchMax
	}

	now := uint64(time.Now().Unix())
	urls := make([]common.PresignedUrl, 0, count)
	for i := uint32(0); i < count; i++ {
		segmentID := uuid.New().String()
		s3Key := s3.SegmentKey(deviceID, segmentID)
		putURL, err := a.S3.PresignPut(ctx, s3Key)
		if err != nil {
			slog.Error("presign PUT failed", "device_id", deviceID, "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		urls = append(urls, common.PresignedUrl{
			SegmentID: segmentID,
			S3Key:     s3Key,
			PutURL:    putURL,
			ExpiresAt: now + a.Config.S3PresignTTLSecs,
		})
	}

	// 5. Check if init segment needs uploading.
	var initURL *common.PresignedUrl
	latest, _ := a.DB.LatestSegment(ctx, deviceID)
	if latest == nil {
		initKey := s3.InitKey(deviceID)
		if putURL, err := a.S3.PresignPut(ctx, initKey); err == nil {
			initURL = &common.PresignedUrl{
				SegmentID: "init",
				S3Key:     initKey,
				PutURL:    putURL,
				ExpiresAt: now + a.Config.S3PresignTTLSecs,
			}
		}
	}

	writeJSON(w, http.StatusOK, common.PresignResponse{URLs: urls, InitURL: initURL})
}

// getUserStorageCached returns the user's total segment storage in bytes.
// Uses a Redis counter (TTL 5 min) to avoid the expensive SUM+JOIN on every call.
// Falls back to DB on Redis miss or unavailability.
func (a *App) getUserStorageCached(ctx context.Context, userID string) (uint64, error) {
	if a.Redis != nil {
		storageKey := "storage_bytes:" + userID
		val, err := a.Redis.Get(ctx, storageKey).Int64()
		if err == nil && val >= 0 {
			return uint64(val), nil
		}
		if err != goredis.Nil {
			slog.Debug("presign: redis storage cache miss", "error", err)
		}

		dbVal, dbErr := a.DB.GetUserStorageBytes(ctx, userID)
		if dbErr != nil {
			return 0, dbErr
		}
		a.Redis.Set(ctx, storageKey, int64(dbVal), 5*time.Minute)
		return dbVal, nil
	}
	return a.DB.GetUserStorageBytes(ctx, userID)
}
