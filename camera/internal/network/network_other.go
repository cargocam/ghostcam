//go:build !linux

package network

import (
	"context"
	"time"
)

// EnsureWifi is a no-op on non-Linux platforms.
func EnsureWifi(_ context.Context, _ string, _ *string) error {
	return nil
}

// WaitForRoute returns immediately on non-Linux platforms (always has a route).
func WaitForRoute(_ context.Context) {}

// WaitForRouteTimeout returns true immediately on non-Linux platforms.
func WaitForRouteTimeout(_ context.Context, _ time.Duration) bool { return true }
