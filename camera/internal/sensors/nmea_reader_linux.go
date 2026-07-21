//go:build linux

package sensors

// Direct-serial GPS fallback. Reads NMEA straight off /dev/ttyUSB1 and
// feeds the shared fix cache (the same one gpsdQuery reads) whenever gpsd
// is NOT managing the port. See nmea.go for the why; this file owns the
// serial I/O and the gpsd-vs-direct arbitration.

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"time"
)

const (
	// The SIM7600's NMEA port (udev ships it MODE=0666, so the non-root
	// daemon can open it). Matches /etc/default/gpsd's DEVICES= line.
	nmeaSerialPort = "/dev/ttyUSB1"

	// How often the reader re-checks whether it may hold the port, both
	// while idle (waiting to take over) and while reading (to yield if
	// gpsd attaches).
	nmeaArbitrationInterval = 2 * time.Second
)

// startDirectNMEAReader runs until ctx is cancelled. It stays dormant
// while gpsd owns the port (gpsdHasDevice) and only opens the serial
// device directly when gpsd has no device attached — reading the same
// tty from two processes would split the byte stream and garble both.
func startDirectNMEAReader(ctx context.Context) {
	// Self-gate: no serial device (synthetic build, Docker, non-SIM7600
	// hardware) → nothing to do, ever.
	if _, err := os.Stat(nmeaSerialPort); err != nil {
		slog.Debug("gps: no direct NMEA port, direct reader disabled", "port", nmeaSerialPort)
		return
	}

	for {
		if ctx.Err() != nil {
			return
		}
		if gpsdOwnsDevice() {
			// gpsd has the port — stand down and re-check shortly.
			if !sleepCtx(ctx, nmeaArbitrationInterval) {
				return
			}
			continue
		}
		if err := readNMEAOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Debug("gps: direct NMEA read ended", "err", err)
		}
		if !sleepCtx(ctx, nmeaArbitrationInterval) {
			return
		}
	}
}

// readNMEAOnce opens the port and streams fixes into the cache until the
// port errors, gpsd takes over the device, or ctx is cancelled.
func readNMEAOnce(ctx context.Context) error {
	f, err := os.OpenFile(nmeaSerialPort, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// Unblock a parked Read on ctx cancel or when gpsd claims the device.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(nmeaArbitrationInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				_ = f.Close()
				return
			case <-t.C:
				if gpsdOwnsDevice() {
					slog.Debug("gps: gpsd took the device, yielding direct NMEA reader")
					_ = f.Close()
					return
				}
			}
		}
	}()

	slog.Info("gps: reading NMEA directly (gpsd has no device)", "port", nmeaSerialPort)
	var p nmeaParser
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		fix, ok := p.feed(sc.Text())
		if !ok {
			continue
		}
		gpsdMu.Lock()
		gpsdLast = gpsdFix{
			lat:     fix.lat,
			lon:     fix.lon,
			alt:     fix.alt,
			mode:    fix.mode,
			updated: time.Now(),
		}
		gpsdReady = true
		gpsdMu.Unlock()
	}
	return sc.Err()
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
