//go:build !linux || synthetic

package main

import "context"

// ReadModem is a no-op on non-linux / synthetic builds. The host dev
// machine doesn't have a cellular modem; synthetic builds shouldn't
// shell out to mmcli even when on linux.
func ReadModem(_ context.Context) ModemSample {
	return ModemSample{}
}
