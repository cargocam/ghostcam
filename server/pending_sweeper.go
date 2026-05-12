package main

import (
	"context"
	"log/slog"
	"time"
)

// PendingTTLSeconds is how long a row stays in the pending state
// before the sweeper drops it. Chosen so a normal cellular upload
// (10–30 s) never trips the timeout, but a camera that dies
// mid-upload doesn't leave a "blue ghost" on the server side forever.
// The UI has its own ~60 s client-side fade-out so the operator's
// view recovers faster than the DB row does — by the time the
// sweeper fires here, the indicator has long since faded.
const PendingTTLSeconds = 5 * 60

// runPendingSegmentSweeper periodically drops pending-but-never-
// confirmed rows. No corrective SSE — the UI auto-fades its own
// pending overlay after ~60 s of silence on a per-segment basis,
// so we'd just be duplicating signal. Pairs with `InsertSegments`'
// ON CONFLICT clear-pending: together they form the lifecycle
// (insert pending → confirm via uploaded → row cleared, OR insert
// pending → no confirm within 5 min → row deleted).
func runPendingSegmentSweeper(ctx context.Context, a *App) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoffMs := uint64(time.Now().Add(-PendingTTLSeconds*time.Second).UnixMilli())
			ids, err := a.DB.PrunePendingSegments(ctx, cutoffMs)
			if err != nil {
				slog.Warn("pending sweeper: prune failed", "error", err)
				continue
			}
			if len(ids) > 0 {
				slog.Info("pending sweeper: dropped expired rows", "count", len(ids))
			}
		}
	}
}
