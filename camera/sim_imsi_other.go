//go:build !linux || synthetic

package main

import "context"

// ReadSIMImsi is a no-op stub on non-Linux builds and synthetic
// (Docker test) builds. Real mmcli-backed lookup lives in
// sim_imsi_linux.go and is built only with `linux && !synthetic`.
// Returns "" so the wire field is omitted via omitempty.
func ReadSIMImsi(_ context.Context) string { return "" }
