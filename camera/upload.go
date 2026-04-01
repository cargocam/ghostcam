package camera

import (
	"context"
	"log/slog"
	"os"

	"github.com/cargocam/ghostcam/api"
)

// RunUploadLoop consumes segments from the channel, uploads them via presigned
// URLs, confirms uploads with the server, and deletes local files.
func RunUploadLoop(ctx context.Context, client *Client, segments <-chan NewSegment) {
	var availableURLs []api.PresignedUrl
	var confirmations []api.UploadedSegment

	for {
		select {
		case <-ctx.Done():
			return
		case seg, ok := <-segments:
			if !ok {
				return
			}
			uploadSegment(ctx, client, seg, &availableURLs, &confirmations)
		}
	}
}

func uploadSegment(
	ctx context.Context,
	client *Client,
	seg NewSegment,
	availableURLs *[]api.PresignedUrl,
	confirmations *[]api.UploadedSegment,
) {
	// Get a presigned URL (request more if we're out)
	if len(*availableURLs) == 0 {
		if err := replenishURLs(ctx, client, availableURLs, confirmations); err != nil {
			slog.Warn("failed to get presigned URLs", "err", err)
			return
		}
	}

	if len(*availableURLs) == 0 {
		slog.Warn("no presigned URLs available, skipping segment", "file", seg.Filename)
		return
	}

	// Pop the first URL
	presigned := (*availableURLs)[0]
	*availableURLs = (*availableURLs)[1:]

	// Read segment data from disk
	data, err := os.ReadFile(seg.Path)
	if err != nil {
		slog.Warn("failed to read segment file", "file", seg.Filename, "err", err)
		return
	}

	// Upload to S3
	if err := client.UploadFile(ctx, presigned.PutURL, data); err != nil {
		slog.Warn("S3 upload failed", "segment_id", seg.Filename, "err", err)
		return
	}

	slog.Debug("segment uploaded to S3", "segment_id", presigned.SegmentID)

	// Queue confirmation for next presign request
	*confirmations = append(*confirmations, api.UploadedSegment{
		SegmentID: presigned.SegmentID,
		StartTS:   seg.StartTS,
		EndTS:     seg.EndTS,
		SizeBytes: seg.SizeBytes,
	})

	// Delete local file
	if err := os.Remove(seg.Path); err != nil {
		slog.Debug("failed to delete uploaded segment", "file", seg.Filename, "err", err)
	}
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

	*availableURLs = append(*availableURLs, resp.URLs...)
	return nil
}
