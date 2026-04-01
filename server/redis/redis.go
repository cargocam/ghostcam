// Package redis provides Redis client and telemetry operations.
package redis

import (
	"context"
	"fmt"
	"log/slog"

	goredis "github.com/redis/go-redis/v9"
)

// Client wraps a go-redis client.
type Client struct {
	rdb *goredis.Client
}

// NewClient creates a new Redis client from a URL.
func NewClient(url string) (*Client, error) {
	opts, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parsing Redis URL: %w", err)
	}

	rdb := goredis.NewClient(opts)
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Warn("redis ping failed (will retry on use)", "error", err)
	}

	return &Client{rdb: rdb}, nil
}

// Close closes the Redis connection.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// RDB returns the underlying go-redis client for direct use.
func (c *Client) RDB() *goredis.Client {
	return c.rdb
}
