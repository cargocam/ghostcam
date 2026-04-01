package camera

import (
	"crypto/rand"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/cargocam/ghostcam/api"
)

// nowMillis returns the current time as Unix milliseconds.
func nowMillis() uint64 {
	return uint64(time.Now().UnixMilli())
}

// generateAndStoreSerial generates a random UUID serial and persists it.
func generateAndStoreSerial(dataDir string) string {
	serial := generateUUID()
	_ = os.WriteFile(dataDir+"/device_serial", []byte(serial), 0644)
	return serial
}

// InjectSyntheticGPS adds synthetic GPS coordinates that drift around Seattle.
func InjectSyntheticGPS(d *api.TelemetryDatagram) {
	t := float64(time.Now().UnixMilli()) / 1000.0
	lat := 47.6062 + 0.001*math.Sin(t/60.0)
	lon := -122.3321 + 0.001*math.Cos(t/45.0)
	alt := float32(50.0)
	fix := uint8(3)
	d.Lat = &lat
	d.Lon = &lon
	d.Alt = &alt
	d.GPSFix = &fix
}

// generateUUID produces a v4 UUID without external dependencies.
func generateUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
