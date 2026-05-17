//go:build !linux || synthetic

package main

import (
	"context"
	"errors"
)

// NewPiSugar3Reader is a stub on non-Linux / synthetic builds. The
// real driver talks to /dev/i2c-1 and is gated to production builds
// only — synthetic / host-arch dev binaries have no I²C bus.
func NewPiSugar3Reader(_ context.Context, _ string) (BatteryReader, error) {
	return nil, errors.New("pisugar3 driver requires linux build (real hardware only)")
}
