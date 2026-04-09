package camera

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/cargocam/ghostcam/common"
)

const (
	maxUploadRetries = 3
	pendingFile      = "pending_confirms.json"
)

// loadPendingConfirms reads any confirmations persisted from a previous run.
// Returns nil on any error (missing file, corrupt, etc.) -- the worst case is
// that the server never gets the confirm and the segment becomes an orphan,
// which is exactly the state we'd be in without persistence.
func loadPendingConfirms(dataDir string) []common.UploadedSegment {
	if dataDir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dataDir, pendingFile))
	if err != nil {
		return nil
	}
	var out []common.UploadedSegment
	if err := json.Unmarshal(data, &out); err != nil {
		slog.Warn("corrupt pending_confirms.json, discarding", "err", err)
		return nil
	}
	return out
}

// savePendingConfirms writes the confirm queue atomically (tmp + rename).
// Called after any mutation so a crash between PUT and confirm doesn't orphan
// uploaded S3 objects.
func savePendingConfirms(dataDir string, confirms []common.UploadedSegment) {
	if dataDir == "" {
		return
	}
	path := filepath.Join(dataDir, pendingFile)
	data, err := json.Marshal(confirms)
	if err != nil {
		slog.Warn("failed to marshal pending confirms", "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		slog.Warn("failed to write pending confirms", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Warn("failed to rename pending confirms", "err", err)
	}
}

// RunUploadLoop consumes segments from the channel, uploads them via presigned
// URLs, confirms uploads with the server, and deletes local files.
// On upload failure, segments are re-enqueued up to maxUploadRetries times.
//
// Pending confirmations are persisted to {dataDir}/pending_confirms.json so a
// crash or restart between the S3 PUT and the confirming presign request does
// not leave orphaned objects in the bucket.
func RunUploadLoop(ctx context.Context, client *Client, dataDir string, segments <-chan NewSegment) {
	var availableURLs []common.PresignedUrl
	confirmations := loadPendingConfirms(dataDir)
	if len(confirmations) > 0 {
		slog.Info("resuming pending upload confirmations", "count", len(confirmations))
	}

	// retryQueue holds segments that failed to upload and need retry
	var retryQueue []NewSegment

	for {
		// Process retry queue first
		if len(retryQueue) > 0 {
			seg := retryQueue[0]
			retryQueue = retryQueue[1:]

			// Exponential backoff: 2s, 4s, 8s
			backoff := time.Duration(1<<uint(seg.RetryCount)) * 2 * time.Second
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}

			if failed := uploadSegmentWithRetry(ctx, client, dataDir, seg, &availableURLs, &confirmations); failed != nil {
				retryQueue = append(retryQueue, *failed)
			}
			continue
		}

		select {
		case <-ctx.Done():
			// Flush pending confirmations on shutdown
			if len(confirmations) > 0 {
				flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := client.RequestPresignedURLs(flushCtx, 0, confirmations)
				flushCancel()
				if err == nil {
					savePendingConfirms(dataDir, nil)
				}
			}
			return
		case seg, ok := <-segments:
			if !ok {
				return
			}
			if failed := uploadSegmentWithRetry(ctx, client, dataDir, seg, &availableURLs, &confirmations); failed != nil {
				retryQueue = append(retryQueue, *failed)
			}
		}
	}
}

// uploadSegmentWithRetry attempts to upload a segment. If upload fails and
// retries remain, it returns the segment with incremented retry count.
func uploadSegmentWithRetry(
	ctx context.Context,
	client *Client,
	dataDir string,
	seg NewSegment,
	availableURLs *[]common.PresignedUrl,
	confirmations *[]common.UploadedSegment,
) *NewSegment {
	ok := uploadSegment(ctx, client, dataDir, seg, availableURLs, confirmations)
	if ok {
		return nil
	}
	if seg.RetryCount >= maxUploadRetries {
		slog.Error("S3 upload failed after max retries, skipping (segment stays on disk)",
			"file", seg.Filename, "retries", seg.RetryCount)
		return nil
	}
	seg.RetryCount++
	slog.Warn("S3 upload failed, will retry",
		"file", seg.Filename, "retry", seg.RetryCount, "max", maxUploadRetries)
	return &seg
}

// storageCapped tracks whether the server has indicated storage is full.
var storageCapped atomic.Bool

// serverUnreachable is set after repeated presign failures so the capture
// pipeline can pause instead of writing segments that will be evicted.
var serverUnreachable atomic.Bool
var presignFailCount atomic.Int32

// IsServerUnreachable returns true when the upload loop has failed to reach
// the server for multiple consecutive presign requests.
func IsServerUnreachable() bool {
	return serverUnreachable.Load()
}

// uploadSegment attempts to upload a single segment. Returns true on success,
// false if the S3 upload failed and the segment should be retried.
func uploadSegment(
	ctx context.Context,
	client *Client,
	dataDir string,
	seg NewSegment,
	availableURLs *[]common.PresignedUrl,
	confirmations *[]common.UploadedSegment,
) bool {
	// Get a presigned URL (request more if we're out)
	if len(*availableURLs) == 0 {
		if err := replenishURLs(ctx, client, dataDir, availableURLs, confirmations); err != nil {
			slog.Warn("failed to get presigned URLs", "err", err)
			return false
		}
	}

	if storageCapped.Load() {
		slog.Debug("storage capped, keeping segment locally", "file", seg.Filename)
		return true // not a retriable failure
	}

	if len(*availableURLs) == 0 {
		slog.Warn("no presigned URLs available, skipping segment", "file", seg.Filename)
		return false
	}

	// Pop the first URL
	presigned := (*availableURLs)[0]
	*availableURLs = (*availableURLs)[1:]

	// Read segment data from disk
	data, err := os.ReadFile(seg.Path)
	if err != nil {
		slog.Warn("failed to read segment file", "file", seg.Filename, "err", err)
		return true // file gone, no point retrying
	}

	// Upload to S3
	if err := client.UploadFile(ctx, presigned.PutURL, data); err != nil {
		slog.Warn("S3 upload failed", "segment_id", seg.Filename, "err", err)
		// On 4xx (expired/invalid URL), discard all cached URLs so the next
		// attempt gets fresh presigned URLs. Don't burn a retry.
		if s3Err, ok := err.(*S3UploadError); ok && s3Err.IsClientError() {
			*availableURLs = nil
		}
		return false
	}

	slog.Debug("segment uploaded to S3", "segment_id", presigned.SegmentID)

	// Queue confirmation for next presign request and persist immediately so a
	// crash before the next presign request doesn't orphan this S3 object.
	*confirmations = append(*confirmations, common.UploadedSegment{
		SegmentID: presigned.SegmentID,
		StartTS:   seg.StartTS,
		EndTS:     seg.EndTS,
		SizeBytes: seg.SizeBytes,
		HasMotion: seg.HasMotion,
	})
	savePendingConfirms(dataDir, *confirmations)

	// Delete local file
	if err := os.Remove(seg.Path); err != nil {
		slog.Debug("failed to delete uploaded segment", "file", seg.Filename, "err", err)
	}
	return true
}

func replenishURLs(
	ctx context.Context,
	client *Client,
	dataDir string,
	availableURLs *[]common.PresignedUrl,
	confirmations *[]common.UploadedSegment,
) error {
	pending := *confirmations
	*confirmations = nil

	resp, err := client.RequestPresignedURLs(ctx, 3, pending)
	if err != nil {
		// Put confirmations back so they aren't lost (on-disk copy is still intact)
		*confirmations = pending
		if n := presignFailCount.Add(1); n >= 3 {
			serverUnreachable.Store(true)
		}
		return err
	}

	// Server reachable — reset failure tracking
	presignFailCount.Store(0)
	if serverUnreachable.Load() {
		slog.Info("server reachable again, resuming capture")
		serverUnreachable.Store(false)
	}

	// Server accepted the confirmations; clear the on-disk queue.
	if len(pending) > 0 {
		savePendingConfirms(dataDir, nil)
	}

	if resp.StorageCapped {
		if !storageCapped.Load() {
			slog.Warn("storage capped by server, pausing uploads and retaining local segments")
		}
		storageCapped.Store(true)
		return nil
	}

	// Clear capped state if we got URLs
	if storageCapped.Load() && len(resp.URLs) > 0 {
		slog.Info("storage cap cleared, resuming uploads")
		storageCapped.Store(false)
	}

	*availableURLs = append(*availableURLs, resp.URLs...)
	return nil
}
