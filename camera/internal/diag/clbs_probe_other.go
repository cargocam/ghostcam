//go:build !linux || synthetic

package diag

import "context"

// probeCLBS is a no-op off real hardware — no modem AT port to talk to.
func probeCLBS(_ context.Context) string { return "" }
