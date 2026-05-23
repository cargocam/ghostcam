//go:build !linux || synthetic

package sensors

import (
	"math"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/audio"
	"github.com/cargocam/ghostcam/camera/internal/battery"
	"github.com/cargocam/ghostcam/camera/internal/capture"
	"github.com/cargocam/ghostcam/camera/internal/upload"
	"github.com/cargocam/ghostcam/common"
)

// GetDeviceSerial returns a stored or generated UUID on non-Linux platforms.
func GetDeviceSerial(dataDir string) string {
	if s := readTrimmedFile(dataDir + "/device_serial"); s != "" {
		return s
	}
	return generateAndStoreSerial(dataDir)
}

// ReadTelemetry returns synthetic sensor values for development.
func ReadTelemetry() common.TelemetryDatagram {
	uptime := uint32(time.Since(time.Unix(0, 0)).Seconds()) % 86400
	cpu := uint32(15)
	mem := uint32(256)
	temp := uint32(45)
	sig := int8(-55)

	// Synthetic GPS: orbits around Seattle with per-device offset from gpsSeed.
	// Uses 3 offset slots so cameras cluster into groups: two nearby + one far.
	h := uint64(0)
	for _, b := range []byte(gpsSeed) {
		h = h*31 + uint64(b)
	}
	phaseOffset := float64(h%10000) / 10000.0 * 2 * math.Pi
	// Slot 0,1 = close together near downtown Seattle (~1km apart).
	// Slot 2 = visually separated (~10 km east) to test the multi-cluster
	// map-fitBounds + offset-angle logic. Slot 2 was previously
	// `{0.15, -0.10}` which placed cameras at (47.756, -122.427) — right
	// in the middle of Puget Sound. On the Carto dark basemap that
	// renders as featureless near-black water and the map LOOKS broken
	// at first glance. Moved to `{0.10, +0.20}` which lands in
	// Bellevue / Mercer Island (densely-populated land) so the map
	// looks alive against any theme.
	slot := h % 3
	offsets := [][2]float64{{0.002, 0.003}, {0.004, 0.001}, {0.10, 0.20}}
	latOffset := offsets[slot][0]
	lonOffset := offsets[slot][1]

	t := float64(time.Now().UnixMilli()) / 1000.0
	lat := 47.6062 + latOffset + 0.005*math.Sin(t/120.0+phaseOffset)
	lon := -122.3321 + lonOffset + 0.005*math.Cos(t/90.0+phaseOffset)
	alt := float32(50.0)
	gpsFix := uint8(3)

	d := common.TelemetryDatagram{
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
	// Surface the active rpicam-vid parameters (#113) — docker-compose
	// test cameras populate this from the test pipeline so the UI
	// behaves identically against synthetic and real fleets.
	if w, h, br, ok := capture.CurrentCaptureParams(); ok {
		d.CurrentWidth = common.PtrUint32(w)
		d.CurrentHeight = common.PtrUint32(h)
		d.CurrentBitrateKbps = common.PtrUint32(br / 1000)
	}
	if n, ok := upload.SampleLocalSegmentBacklog(); ok {
		d.LocalSegmentBacklog = common.PtrUint32(n)
	}
	if pct := battery.ReadBatteryPct(); pct != nil {
		d.BatteryPct = pct
	}
	if dbfs := audio.ReadAudioRMSDBFS(); dbfs != nil {
		d.AudioRMSDBFS = dbfs
	}
	return d
}
