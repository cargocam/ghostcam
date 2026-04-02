package camera

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/cargocam/ghostcam/api"
)

const maxUploadRetries = 3

// RunUploadLoop consumes segments from the channel, uploads them via presigned
// URLs, confirms uploads with the server, and deletes local files.
// On upload failure, segments are re-enqueued up to maxUploadRetries times.
func RunUploadLoop(ctx context.Context, client *Client, segments <-chan NewSegment) {
	var availableURLs []api.PresignedUrl
	var confirmations []api.UploadedSegment

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

			if failed := uploadSegmentWithRetry(ctx, client, seg, &availableURLs, &confirmations); failed != nil {
				retryQueue = append(retryQueue, *failed)
			}
			continue
		}

		select {
		case <-ctx.Done():
			// Flush pending confirmations on shutdown
			if len(confirmations) > 0 {
				flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, _ = client.RequestPresignedURLs(flushCtx, 0, confirmations)
				flushCancel()
			}
			return
		case seg, ok := <-segments:
			if !ok {
				return
			}
			if failed := uploadSegmentWithRetry(ctx, client, seg, &availableURLs, &confirmations); failed != nil {
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
	seg NewSegment,
	availableURLs *[]api.PresignedUrl,
	confirmations *[]api.UploadedSegment,
) *NewSegment {
	ok := uploadSegment(ctx, client, seg, availableURLs, confirmations)
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

// uploadSegment attempts to upload a single segment. Returns true on success,
// false if the S3 upload failed and the segment should be retried.
func uploadSegment(
	ctx context.Context,
	client *Client,
	seg NewSegment,
	availableURLs *[]api.PresignedUrl,
	confirmations *[]api.UploadedSegment,
) bool {
	// Get a presigned URL (request more if we're out)
	if len(*availableURLs) == 0 {
		if err := replenishURLs(ctx, client, availableURLs, confirmations); err != nil {
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
		return false
	}

	slog.Debug("segment uploaded to S3", "segment_id", presigned.SegmentID)

	// Queue confirmation for next presign request
	*confirmations = append(*confirmations, api.UploadedSegment{
		SegmentID: presigned.SegmentID,
		StartTS:   seg.StartTS,
		EndTS:     seg.EndTS,
		SizeBytes: seg.SizeBytes,
		HasMotion: seg.HasMotion,
	})

	// Delete local file
	if err := os.Remove(seg.Path); err != nil {
		slog.Debug("failed to delete uploaded segment", "file", seg.Filename, "err", err)
	}
	return true
}

func replenishURLs(
	ctx context.Context,
	client *Client,
	availableURLs *[]api.PresignedUrl,
	confirmations *[]api.UploadedSegment,
) error {
	pending := *confirmations
	*confirmations = nil

	resp, err := client.RequestPresignedURLs(ctx, 3, pending)
	if err != nil {
		// Put confirmations back so they aren't lost
		*confirmations = pending
		return err
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
