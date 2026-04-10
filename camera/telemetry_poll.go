package main

import (
	"context"
	"log/slog"
	"time"
)

// RunTelemetryPoll sends telemetry to the server every 10s, processes
// piggy-backed commands, and backs off on consecutive failures.
func RunTelemetryPoll(ctx context.Context, client *Client, dataDir string) {
	const (
		baseInterval = 10 * time.Second
		maxInterval  = 60 * time.Second
	)
	interval := baseInterval
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			telemetry := ReadTelemetry()
			commands, err := client.PostTelemetry(ctx, telemetry)
			if err != nil {
				consecutiveFailures++
				slog.Debug("telemetry POST failed", "err", err, "consecutive_failures", consecutiveFailures)
				switch {
				case consecutiveFailures >= 3:
					interval = maxInterval
				case consecutiveFailures >= 2:
					interval = 30 * time.Second
				default:
					interval = baseInterval
				}
				continue
			}
			if consecutiveFailures > 0 {
				consecutiveFailures = 0
				interval = baseInterval
			}
			for _, cmd := range commands {
				HandleCommand(ctx, cmd, dataDir)
			}
		}
	}
}
