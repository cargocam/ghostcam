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

// EnsureCellular is a no-op on non-Linux platforms.
func EnsureCellular(_ context.Context, _, _, _ string) error {
	return nil
}

// SetWifiRadio is a no-op on non-Linux platforms.
func SetWifiRadio(_ context.Context, _ bool) error {
	return nil
}

// WaitForRoute returns immediately on non-Linux platforms (always has a route).
func WaitForRoute(_ context.Context) {}

// WaitForRouteTimeout returns true immediately on non-Linux platforms.
func WaitForRouteTimeout(_ context.Context, _ time.Duration) bool { return true }

// DefaultInterface returns "" on non-Linux platforms (no /proc/net/route).
func DefaultInterface() string { return "" }
