//go:build linux

package camera

import (
	"os"
	"strconv"
	"strings"

	"github.com/cargocam/ghostcam/api"
)

// GetDeviceSerial reads the Pi serial from /proc/cpuinfo, falling back to a
// stored or generated UUID.
func GetDeviceSerial(dataDir string) string {
	// Check stored serial first
	if s := readTrimmedFile(dataDir + "/device_serial"); s != "" {
		return s
	}

	// Read Pi serial from /proc/cpuinfo
	data, err := os.ReadFile("/proc/cpuinfo")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Serial") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					serial := strings.TrimSpace(parts[1])
					if serial != "" {
						_ = os.WriteFile(dataDir+"/device_serial", []byte(serial), 0644)
						return serial
					}
				}
			}
		}
	}

	return generateAndStoreSerial(dataDir)
}

// ReadTelemetry reads CPU, memory, temperature, uptime, and WiFi signal from
// /proc and /sys on Linux.
func ReadTelemetry() api.TelemetryDatagram {
	d := api.TelemetryDatagram{
		TS: nowMillis(),
	}

	if cpu := readCPU(); cpu != nil {
		d.CPU = cpu
	}
	if mem := readMemory(); mem != nil {
		d.Mem = mem
	}
	if temp := readTemperature(); temp != nil {
		d.Temp = temp
	}
	if up := readUptime(); up != nil {
		d.Uptime = up
	}
	if sig := readWifiSignal(); sig != nil {
		d.Sig = sig
	}

	// GPS: try gpsd first, fall back to synthetic for dev/Docker
	if lat, lon, alt, fix := readGPS(); lat != nil {
		d.Lat = lat
		d.Lon = lon
		d.Alt = alt
		d.GPSFix = fix
	}

	return d
}

func readCPU() *uint32 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 { // "cpu" + at least 4 numbers
		return nil
	}
	var total, idle uint64
	for i, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			continue
		}
		total += v
		if i == 3 { // idle is the 4th field
			idle = v
		}
	}
	if total == 0 {
		v := uint32(0)
		return &v
	}
	v := uint32((total - idle) * 100 / total)
	return &v
}

func readMemory() *uint32 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	var total, available uint64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			total = parseMemInfoValue(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			available = parseMemInfoValue(line)
		}
	}
	if total == 0 {
		return nil
	}
	v := uint32((total - available) / 1024) // kB -> MB
	return &v
}

func parseMemInfoValue(line string) uint64 {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(parts[1], 10, 64)
	return v
}

func readTemperature() *uint32 {
	data, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return nil
	}
	millideg, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return nil
	}
	v := uint32(millideg / 1000)
	return &v
}

func readUptime() *uint32 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return nil
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil
	}
	v := uint32(secs)
	return &v
}

// readGPS queries gpsd for a GPS fix. Returns nils if gpsd is unavailable.
func readGPS() (*float64, *float64, *float32, *uint8) {
	return gpsdQuery()
}

func readWifiSignal() *int8 {
	data, err := os.ReadFile("/proc/net/wireless")
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 3 {
		return nil
	}
	// Third line has the data
	fields := strings.Fields(lines[2])
	if len(fields) < 4 {
		return nil
	}
	level, err := strconv.ParseFloat(strings.TrimRight(fields[3], "."), 64)
	if err != nil {
		return nil
	}
	v := int8(level)
	return &v
}
