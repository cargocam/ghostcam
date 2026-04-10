// Package redis provides helper functions over a standard go-redis client
// for telemetry streams, event storage, and pub/sub used by the server.
package redis

import (
	"context"
	"fmt"
	"log/slog"

	goredis "github.com/redis/go-redis/v9"
)

// Connect parses the URL and returns a ready-to-use go-redis client.
// Ping failures are logged but not fatal — the server treats Redis as
// optional and degrades gracefully when it is unreachable.
func Connect(url string) (*goredis.Client, error) {
	opts, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parsing Redis URL: %w", err)
	}

	rdb := goredis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		slog.Warn("redis ping failed (will retry on use)", "error", err)
	}
	return rdb, nil
}
