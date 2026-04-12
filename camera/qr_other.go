//go:build !linux || synthetic

package main

import (
	"context"

	"github.com/cargocam/ghostcam/common"
)

// ScanQR is a no-op on non-Linux or synthetic platforms (no camera hardware).
func ScanQR(_ context.Context) (*common.QRPayload, error) { return nil, nil }
