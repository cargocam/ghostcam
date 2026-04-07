//go:build !linux || synthetic

package camera

import (
	"math"
	"time"

	"github.com/cargocam/ghostcam/api"
)

// GetDeviceSerial returns a stored or generated UUID on non-Linux platforms.
func GetDeviceSerial(dataDir string) string {
	if s := readTrimmedFile(dataDir + "/device_serial"); s != "" {
		return s
	}
	return generateAndStoreSerial(dataDir)
}

// ReadTelemetry returns synthetic sensor values for development.
func ReadTelemetry() api.TelemetryDatagram {
	uptime := uint32(time.Since(time.Unix(0, 0)).Seconds()) % 86400
	cpu := uint32(15)
	mem := uint32(256)
	temp := uint32(45)
	sig := int8(-55)

	// Synthetic GPS: orbits around Seattle with per-device offset from gpsSeed
	h := uint64(0)
	for _, b := range []byte(gpsSeed) {
		h = h*31 + uint64(b)
	}
	phaseOffset := float64(h%10000) / 10000.0 * 2 * math.Pi
	latOffset := float64(h%1000) / 1000.0 * 0.02
	lonOffset := float64(h%997) / 997.0 * 0.02

	t := float64(time.Now().UnixMilli()) / 1000.0
	lat := 47.6062 + latOffset + 0.005*math.Sin(t/120.0+phaseOffset)
	lon := -122.3321 + lonOffset + 0.005*math.Cos(t/90.0+phaseOffset)
	alt := float32(50.0)
	gpsFix := uint8(3)

	return api.TelemetryDatagram{
		TS:     nowMillis(),
		CPU:    &cpu,
		Mem:    &mem,
		Temp:   &temp,
		Uptime: &uptime,
		Sig:    &sig,
		Lat:    &lat,
		Lon:    &lon,
		Alt:    &alt,
		GPSFix: &gpsFix,
	}
}
