package camera

import (
	"crypto/rand"
	"fmt"
	"os"
	"time"
)

// nowMillis returns the current time as Unix milliseconds.
func nowMillis() uint64 {
	return uint64(time.Now().UnixMilli())
}

// gpsSeed is set at startup from the device serial so synthetic GPS
// positions are deterministic and unique per device.
var gpsSeed string

// SetGPSSeed sets the device-specific seed used by synthetic GPS.
func SetGPSSeed(seed string) {
	gpsSeed = seed
}

// generateAndStoreSerial generates a random UUID serial and persists it.
func generateAndStoreSerial(dataDir string) string {
	serial := generateUUID()
	_ = os.WriteFile(dataDir+"/device_serial", []byte(serial), 0644)
	return serial
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
