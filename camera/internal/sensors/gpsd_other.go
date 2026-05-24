//go:build !linux

package sensors

import "context"

// gpsdQuery is a no-op on non-Linux platforms.
func gpsdQuery() (*float64, *float64, *float32, *uint8) {
	return nil, nil, nil, nil
}

// StartGpsdReader is a no-op on non-Linux platforms.
func StartGpsdReader(ctx context.Context) {
	<-ctx.Done()
}
