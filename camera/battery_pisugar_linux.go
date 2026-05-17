//go:build linux && !synthetic

package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// PiSugar 3 / 3 Plus battery HAT driver (#73). Talks to the HAT over
// /dev/i2c-1 at slave address 0x57. The HAT's microcontroller exposes
// a small register file; the only register we need for power-mode
// decisions is the battery percentage byte at 0x2A. Register layout is
// documented in PiSugar's `PiSugar-Power-Manager-for-RPi` (see
// model_pisugar3.rs). Voltage / charging / RTC are intentionally
// out of scope here — telemetry only carries battery_pct today and
// extending the wire is a separate change.
//
// Bus access is serialised by a mutex because we share a single fd
// for the lifetime of the reader (open-on-every-read would race with
// any future I²C consumer and waste descriptors). The poller runs at
// 30 s intervals; readers see the most recent sample via the atomic.
// If the HAT goes away (cable knocked loose, brown-out), three
// consecutive failures clear the cached value so telemetry reports
// nil instead of a stale percentage.

const (
	pisugar3I2CAddress  = 0x57
	pisugar3RegBatteryPct = 0x2A
	// I2C_SLAVE ioctl from linux/i2c-dev.h. golang.org/x/sys/unix has
	// this constant but is currently only a transitive dep; declaring
	// it inline keeps the camera module's direct deps narrow.
	i2cSlaveIoctl = 0x0703
	pisugar3PollInterval = 30 * time.Second
	pisugar3MaxConsecutiveFailures = 3
)

type pisugar3Reader struct {
	mu  sync.Mutex
	fd  int
	pct atomic.Pointer[uint8]
}

// NewPiSugar3Reader opens the I²C bus, sets the PiSugar slave address,
// performs one initial read so callers can fail-fast when no HAT is
// wired up, and spawns a poller. The returned reader is safe to
// register via SetBatteryReader.
func NewPiSugar3Reader(ctx context.Context, busPath string) (BatteryReader, error) {
	fd, err := syscall.Open(busPath, syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", busPath, err)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(i2cSlaveIoctl), uintptr(pisugar3I2CAddress)); errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("ioctl I2C_SLAVE 0x%02x: %w", pisugar3I2CAddress, errno)
	}
	r := &pisugar3Reader{fd: fd}
	pct, err := r.readRegister(pisugar3RegBatteryPct)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("initial read: %w", err)
	}
	if pct > 100 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("implausible battery pct %d from PiSugar (HAT firmware?)", pct)
	}
	v := pct
	r.pct.Store(&v)
	go r.runPoller(ctx)
	return r, nil
}

func (r *pisugar3Reader) ReadPct() *uint8 { return r.pct.Load() }

// readRegister writes the target register byte, then reads one byte
// of response. Callers must hold r.mu — or be the only goroutine
// touching the fd, as is the case during NewPiSugar3Reader.
func (r *pisugar3Reader) readRegister(reg byte) (uint8, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := syscall.Write(r.fd, []byte{reg}); err != nil {
		return 0, fmt.Errorf("write reg 0x%02x: %w", reg, err)
	}
	buf := [1]byte{}
	if _, err := syscall.Read(r.fd, buf[:]); err != nil {
		return 0, fmt.Errorf("read reg 0x%02x: %w", reg, err)
	}
	return buf[0], nil
}

func (r *pisugar3Reader) runPoller(ctx context.Context) {
	defer func() {
		r.mu.Lock()
		_ = syscall.Close(r.fd)
		r.fd = -1
		r.mu.Unlock()
	}()
	tk := time.NewTicker(pisugar3PollInterval)
	defer tk.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			pct, err := r.readRegister(pisugar3RegBatteryPct)
			if err != nil {
				failures++
				slog.Debug("PiSugar 3 read failed", "err", err, "consecutive", failures)
				if failures >= pisugar3MaxConsecutiveFailures {
					// Clear cached value so the next telemetry tick
					// reports nil (HAT gone, not stale).
					r.pct.Store(nil)
				}
				continue
			}
			if pct > 100 {
				slog.Warn("PiSugar 3 returned implausible pct, ignoring", "pct", pct)
				continue
			}
			failures = 0
			v := pct
			r.pct.Store(&v)
		}
	}
}

