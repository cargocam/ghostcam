//go:build !linux || synthetic

package main

import (
	"context"

	"github.com/cargocam/ghostcam/common"
)

// ScanBT is a no-op on synthetic and non-Linux builds. The provisioning
// race in provisioning.go treats (nil, nil) as "this source has nothing"
// and falls through to the other sources.
func ScanBT(_ context.Context, _ string) (*common.QRPayload, error) { return nil, nil }
