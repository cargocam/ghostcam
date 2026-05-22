//go:build linux && !synthetic

package main

import (
	"context"
	"log/slog"
	"os/exec"
	"time"
)

// ReadModem queries ModemManager for the current cellular link's RAT
// and signal quality. Returns a zero ModemSample (caller treats as
// "no data") on any failure — mmcli missing, no modem registered,
// command timeout, parse miss. Best-effort; never blocks longer
// than 3 s so the telemetry tick stays on schedule.
func ReadModem(parent context.Context) ModemSample {
	if _, err := exec.LookPath("mmcli"); err != nil {
		return ModemSample{}
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "mmcli", "-m", "0").Output()
	if err != nil {
		slog.Debug("mmcli -m 0 failed", "err", err)
		return ModemSample{}
	}
	return parseMmcliOutput(string(out))
}
