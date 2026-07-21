package sensors

// Minimal NMEA 0183 parser for the direct-serial GPS fallback.
//
// Why this exists: on the SIM7600, gpsd frequently loses a cold-boot race
// for /dev/ttyUSB1 and — with USBAUTO="false" — never re-attaches, so
// gpsd sits with zero devices and the gpsd WATCH stream yields no fix
// forever, even though the GNSS engine is powered and streaming NMEA on
// the port. The daemon runs as the non-root `ghostcam` user and cannot
// re-attach the port to gpsd (`gpsdctl` needs the root-owned control
// socket) nor power the engine (`mmcli --location-enable` is polkit-
// gated). But the udev rule ships /dev/ttyUSB1 as MODE=0666, so the
// daemon *can* open and read it directly. When gpsd has no device, the
// reader in nmea_reader_linux.go opens the port and feeds lines here.
//
// This file is pure (no serial / OS calls) so it unit-tests on any host.

import (
	"strconv"
	"strings"
)

// nmeaFix is a parsed position, shaped to match gpsdFix's fields so the
// direct reader can populate the same cache the gpsd path uses.
type nmeaFix struct {
	lat, lon float64
	alt      float32
	mode     uint8 // 2=2D, 3=3D (mirrors gpsd TPV mode)
}

// nmeaParser folds a stream of NMEA sentences into fixes. GGA carries
// position + altitude + a fix-quality flag but does not distinguish 2D
// from 3D; GSA carries the 2D/3D nav mode. We remember the most recent
// GSA mode and stamp it onto the fix emitted when a fixed GGA arrives.
type nmeaParser struct {
	lastGSAMode uint8 // 0=unknown, 2=2D, 3=3D
}

// feed parses one line. It returns a fix only for a GGA sentence that
// reports an actual fix (quality >= 1); all other sentences update
// internal state (GSA) or are ignored, returning ok=false.
func (p *nmeaParser) feed(line string) (nmeaFix, bool) {
	line = strings.TrimSpace(line)
	if len(line) == 0 || line[0] != '$' {
		return nmeaFix{}, false
	}
	if !verifyNMEAChecksum(line) {
		return nmeaFix{}, false
	}
	// Drop the trailing *HH checksum before splitting fields.
	if star := strings.IndexByte(line, '*'); star >= 0 {
		line = line[:star]
	}
	fields := strings.Split(line, ",")
	if len(fields) == 0 {
		return nmeaFix{}, false
	}
	// fields[0] is like "$GPGGA" / "$GNGSA" — match on the 3-letter
	// sentence type so any talker ID (GP/GN/GL/GA/BD…) is accepted.
	typ := fields[0]
	if len(typ) < 3 {
		return nmeaFix{}, false
	}
	switch typ[len(typ)-3:] {
	case "GSA":
		if m, ok := parseGSAMode(fields); ok {
			p.lastGSAMode = m
		}
		return nmeaFix{}, false
	case "GGA":
		fix, ok := parseGGA(fields)
		if !ok {
			return nmeaFix{}, false
		}
		if p.lastGSAMode != 0 {
			fix.mode = p.lastGSAMode
		} else {
			// No GSA seen yet; a fixed GGA is at least a 2D fix. Assume
			// 3D — the common case outdoors — rather than under-reporting.
			fix.mode = 3
		}
		return fix, true
	}
	return nmeaFix{}, false
}

// parseGGA extracts position + altitude from a GGA sentence, returning
// ok=false when there is no fix (quality field 0 or empty) or a field
// is malformed.
//
// $GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47
//
//	1=time 2=lat 3=N/S 4=lon 5=E/W 6=quality 7=nsat 8=hdop 9=alt 10=M
func parseGGA(f []string) (nmeaFix, bool) {
	if len(f) < 10 {
		return nmeaFix{}, false
	}
	quality, err := strconv.Atoi(strings.TrimSpace(f[6]))
	if err != nil || quality < 1 {
		return nmeaFix{}, false
	}
	lat, ok := parseCoord(f[2], f[3])
	if !ok {
		return nmeaFix{}, false
	}
	lon, ok := parseCoord(f[4], f[5])
	if !ok {
		return nmeaFix{}, false
	}
	var alt float32
	if a, err := strconv.ParseFloat(strings.TrimSpace(f[9]), 32); err == nil {
		alt = float32(a)
	}
	return nmeaFix{lat: lat, lon: lon, alt: alt}, true
}

// parseGSAMode returns the 2D/3D nav mode from a GSA sentence.
//
// $GPGSA,A,3,04,05,...,2.5,1.3,2.1*39  → field 2 is 1=no fix,2=2D,3=3D
func parseGSAMode(f []string) (uint8, bool) {
	if len(f) < 3 {
		return 0, false
	}
	switch strings.TrimSpace(f[2]) {
	case "2":
		return 2, true
	case "3":
		return 3, true
	}
	return 0, false
}

// parseCoord converts an NMEA ddmm.mmmm / dddmm.mmmm coordinate plus a
// hemisphere letter into signed decimal degrees.
func parseCoord(val, hemi string) (float64, bool) {
	val = strings.TrimSpace(val)
	hemi = strings.TrimSpace(hemi)
	if val == "" || hemi == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0, false
	}
	// Degrees are all but the last two integer digits; the rest is minutes.
	deg := float64(int(f / 100))
	minutes := f - deg*100
	dec := deg + minutes/60
	switch hemi {
	case "N", "E":
		return dec, true
	case "S", "W":
		return -dec, true
	}
	return 0, false
}

// verifyNMEAChecksum validates the *HH XOR checksum that terminates a
// well-formed NMEA sentence. Rejects garbled serial lines (partial reads,
// line noise) that would otherwise parse into a bogus position. A line
// with no "*HH" is treated as invalid.
func verifyNMEAChecksum(line string) bool {
	star := strings.IndexByte(line, '*')
	if star < 1 || star+3 > len(line) {
		return false
	}
	want, err := strconv.ParseUint(line[star+1:star+3], 16, 8)
	if err != nil {
		return false
	}
	var sum byte
	// XOR of everything between '$' and '*'.
	for i := 1; i < star; i++ {
		sum ^= line[i]
	}
	return sum == byte(want)
}
