//go:build !linux

package camera

import "context"

// EnsureWifi is a no-op on non-Linux platforms.
func EnsureWifi(_ context.Context, _ string, _ *string) error {
	return nil
}

// WaitForRoute returns immediately on non-Linux platforms (always has a route).
func WaitForRoute(_ context.Context) {}
