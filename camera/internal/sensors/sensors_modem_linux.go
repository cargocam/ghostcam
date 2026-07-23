//go:build linux && !synthetic

package sensors

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

	idx := modemIndex(ctx)
	out, err := exec.CommandContext(ctx, "mmcli", "-m", idx).Output()
	if err != nil {
		slog.Debug("mmcli -m failed", "idx", idx, "err", err)
		return ModemSample{}
	}
	return parseMmcliOutput(string(out))
}

// modemIndex resolves the current modem index via `mmcli -L`. Falls back
// to "0". Needed because the SIM7600 gets a new index after a reset.
func modemIndex(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "mmcli", "-L").Output()
	if err != nil {
		return "0"
	}
	return parseModemIndex(string(out))
}

// ReadCellLocation queries ModemManager's 3GPP location (serving cell)
// via `mmcli -m 0 --location-get`. Best-effort with a 3 s budget;
// returns a zero CellLocation on any failure. Reading location is
// unprivileged (unlike enabling it), so this works from the non-root
// daemon whenever the image/oneshot has 3GPP location enabled.
func ReadCellLocation(parent context.Context) CellLocation {
	if _, err := exec.LookPath("mmcli"); err != nil {
		return CellLocation{}
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()

	idx := modemIndex(ctx)
	out, err := exec.CommandContext(ctx, "mmcli", "-m", idx, "--location-get").Output()
	if err != nil {
		slog.Debug("mmcli --location-get failed", "idx", idx, "err", err)
		return CellLocation{}
	}
	return parseCellLocation(string(out))
}
