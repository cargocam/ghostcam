//go:build !linux

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

// ReadTelemetry returns synthetic sensor values for development on non-Linux.
func ReadTelemetry() api.TelemetryDatagram {
	uptime := uint32(time.Since(time.Unix(0, 0)).Seconds()) % 86400
	cpu := uint32(15)
	mem := uint32(256)
	temp := uint32(45)
	sig := int8(-55)

	// Synthetic GPS: slowly drifts around Seattle
	t := float64(time.Now().UnixMilli()) / 1000.0
	lat := 47.6062 + 0.001*math.Sin(t/60.0)
	lon := -122.3321 + 0.001*math.Cos(t/45.0)
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
