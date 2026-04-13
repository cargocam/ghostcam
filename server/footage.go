package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/go-chi/chi/v5"
)

// footageDeleteBatchSize caps how many segment rows a single DB
// DELETE ... RETURNING statement processes. Matches pruneBatchSize in
// presign.go so the two cleanup paths exert the same peak load on
// Postgres and S3.
const footageDeleteBatchSize = 100

// maxBatchesPerFootageRequest caps how many batches one HTTP call to
// DELETE /api/v1/cameras/{id}/footage processes. 20 * 100 = 2000
// segments per call (~4 GB at ~2 MB each). Larger purges return
// has_more=true and the UI re-calls until drained.
const maxBatchesPerFootageRequest = 20

// maxBatchesPerCameraDelete caps the synchronous S3 purge embedded in
// DeleteCamera / AdminDeleteCamera. Kept higher than the per-request
// cap because camera deletion is a single user action and we'd rather
// reap as many objects as possible before returning; a residual is
// logged and will never be reclaimed (the cameras row is gone by the
// time we'd notice). In practice a single batch is enough for most
// users — this just bounds the tail for power-users with months of
// uninterrupted recording.
const maxBatchesPerCameraDelete = 200

// DeleteFootage handles DELETE /api/v1/cameras/{deviceID}/footage.
//
// Query params (both optional):
//
//	from_ms  start of range to delete, inclusive (default 0)
//	to_ms    end of range to delete, inclusive (default 0 = unbounded)
//
// Omitting both deletes every segment for the camera. The handler
// processes deletions in bounded batches and returns has_more=true when
// the final batch was full — the UI is expected to re-call until
// has_more is false, showing progress.
func (a *App) DeleteFootage(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	camera, ok := a.ownedCamera(w, r, deviceID)
	if !ok {
		return
	}
	if a.S3 == nil {
		writeError(w, http.StatusServiceUnavailable, "S3 not configured")
		return
	}

	fromMs := parseQueryUint64(r, "from_ms", 0)
	toMs := parseQueryUint64(r, "to_ms", 0)
	if toMs != 0 && fromMs > toMs {
		writeError(w, http.StatusBadRequest, "from_ms must be <= to_ms")
		return
	}

	deletedCount, bytesFreed, hasMore, err := a.purgeDeviceFootage(
		r.Context(), deviceID, fromMs, toMs, maxBatchesPerFootageRequest)
	if err != nil {
		slog.Error("delete footage: purge failed", "device_id", deviceID, "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	userID := ""
	if camera.UserID != nil {
		userID = *camera.UserID
	}
	if bytesFreed > 0 && a.Redis != nil && userID != "" {
		a.Redis.DecrBy(r.Context(), "storage_bytes:"+userID, int64(bytesFreed))
	}

	slog.Info("audit", "event_type", "footage_deleted",
		"device_id", deviceID, "initiated_by", getUserID(r),
		"from_ms", fromMs, "to_ms", toMs,
		"deleted_count", deletedCount, "bytes_freed", bytesFreed,
		"has_more", hasMore)

	writeJSON(w, http.StatusOK, apitypes.DeleteFootageResponse{
		DeletedCount: deletedCount,
		BytesFreed:   bytesFreed,
		HasMore:      hasMore,
	})
}

// purgeDeviceFootage loops DeleteSegmentsRange + S3.Delete up to
// maxBatches times. Returns the totals and whether more rows remain
// (signalled by a full final batch). The caller is responsible for
// decrementing the cached storage counter — this function does not
// touch Redis so it is reusable from call sites that already hold a
// user ID.
//
// Individual S3 delete failures are logged and counted toward
// bytesFreed (the row is gone from the DB so the byte count is the
// accounting-truth from the user's perspective). Other S3 failures
// would otherwise leave the storage counter permanently skewed.
func (a *App) purgeDeviceFootage(
	ctx context.Context,
	deviceID string,
	fromMs, toMs uint64,
	maxBatches int,
) (int, uint64, bool, error) {
	if a.S3 == nil {
		return 0, 0, false, nil
	}

	var (
		deletedCount int
		bytesFreed   uint64
		lastBatchLen int
	)
	for batch := 0; batch < maxBatches; batch++ {
		rows, err := a.DB.DeleteSegmentsRange(ctx, deviceID, fromMs, toMs, footageDeleteBatchSize)
		if err != nil {
			return deletedCount, bytesFreed, false, err
		}
		lastBatchLen = len(rows)
		if lastBatchLen == 0 {
			return deletedCount, bytesFreed, false, nil
		}
		for _, seg := range rows {
			if delErr := a.S3.Delete(ctx, seg.S3Key); delErr != nil {
				slog.Warn("delete footage: S3 delete failed",
					"s3_key", seg.S3Key, "error", delErr)
			}
			bytesFreed += seg.SizeBytes
		}
		deletedCount += lastBatchLen
	}
	return deletedCount, bytesFreed, decideHasMore(lastBatchLen, footageDeleteBatchSize), nil
}

// purgeAllFootageForDelete runs purgeDeviceFootage on the whole camera
// with the higher per-delete batch cap, decrements the Redis storage
// counter, and logs a warning if a tail remains. Used by the
// camera-delete paths so removing a camera no longer silently orphans
// its S3 objects.
func (a *App) purgeAllFootageForDelete(ctx context.Context, deviceID, userID string) {
	if a.S3 == nil {
		return
	}
	// Use a bounded context so a slow S3 endpoint can't block camera
	// delete indefinitely. The tail (if any) is logged; it will not be
	// reclaimed because the cameras row is about to go away, but this
	// matches the pre-existing behavior documented in admin_cameras.go.
	purgeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	deletedCount, bytesFreed, hasMore, err := a.purgeDeviceFootage(
		purgeCtx, deviceID, 0, 0, maxBatchesPerCameraDelete)
	if err != nil {
		slog.Warn("camera delete: footage purge errored",
			"device_id", deviceID, "error", err,
			"deleted_count", deletedCount, "bytes_freed", bytesFreed)
	} else if hasMore {
		slog.Warn("camera delete: footage purge left a tail; remaining S3 objects will be orphaned",
			"device_id", deviceID, "deleted_count", deletedCount, "bytes_freed", bytesFreed)
	} else {
		slog.Info("camera delete: footage purged",
			"device_id", deviceID, "deleted_count", deletedCount, "bytes_freed", bytesFreed)
	}

	if bytesFreed > 0 && a.Redis != nil && userID != "" {
		a.Redis.DecrBy(context.Background(), "storage_bytes:"+userID, int64(bytesFreed))
	}
}

// decideHasMore is the pure helper the handler uses to signal the UI
// whether to re-call the endpoint. A full final batch means there may
// be more rows; a partial final batch (including zero) means we hit
// the end of the range.
func decideHasMore(lastBatchLen, batchSize int) bool {
	return lastBatchLen == batchSize
}
