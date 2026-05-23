package battery

import "sync/atomic"

// BatteryReader returns the current state of charge (0-100), or nil
// when the camera has no battery-sensing HAT wired up. Implementations
// must be safe to call from any goroutine.
//
// The default reader (noBatteryReader) always returns nil so grid-
// powered cameras and dev builds never falsely report a battery
// percentage. A real HAT driver swaps the reader in via SetBatteryReader
// at startup once the I²C bus has been opened.
type BatteryReader interface {
	ReadPct() *uint8
}

type noBatteryReader struct{}

func (noBatteryReader) ReadPct() *uint8 { return nil }

var currentBatteryReader atomic.Pointer[BatteryReader]

// SetBatteryReader replaces the active reader. Safe to call concurrently
// with ReadBatteryPct.
func SetBatteryReader(r BatteryReader) {
	currentBatteryReader.Store(&r)
}

// ReadBatteryPct returns the most recent state-of-charge reading via
// the currently installed BatteryReader. Defaults to the no-op reader
// (returns nil) when no driver has been registered.
func ReadBatteryPct() *uint8 {
	if r := currentBatteryReader.Load(); r != nil {
		return (*r).ReadPct()
	}
	return nil
}

// lastBatteryPct caches the most recent telemetry-sampled value so
// command handlers can re-resolve battery rules without re-querying
// the hardware (which might be slow or behind an I²C bus). Stored as
// a *uint8 so we can distinguish "no reading yet" from "0%".
var lastBatteryPct atomic.Pointer[uint8]

// RecordBatteryPct caches the most recent reading. Called from the
// telemetry-poll loop on every tick.
func RecordBatteryPct(pct *uint8) {
	lastBatteryPct.Store(pct)
}

// LastBatteryPct returns the cached value. nil if no reading has
// been captured yet (e.g. boot before first telemetry tick).
func LastBatteryPct() *uint8 {
	return lastBatteryPct.Load()
}

func init() {
	SetBatteryReader(noBatteryReader{})
}
