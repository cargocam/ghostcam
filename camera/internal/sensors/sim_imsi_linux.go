//go:build linux && !synthetic

package sensors

import (
	"context"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ReadSIMImsi tries to read the IMSI of the cellular SIM via
// ModemManager (mmcli). Returns "" on any failure — non-blocking,
// best-effort, never longer than 3 s. Used by Provision() to include
// the IMSI in the provisioning POST so the server can record it on
// the cameras row (#74).
//
// The mmcli output format we parse:
//
//   $ mmcli -m 0
//   ...
//   SIM   |   primary sim path: /org/freedesktop/ModemManager1/SIM/0
//         |              imsi: 310260123456789
//         |              iccid: 89882200001234567890
//   ...
//
// We look for a line of the form `imsi: <14-15 digit string>` and
// return the digits. Anything else (no modem, mmcli missing, no
// SIM, parse miss) → empty string.
func ReadSIMImsi(parent context.Context) string {
	if _, err := exec.LookPath("mmcli"); err != nil {
		return "" // grid-powered / WiFi-only camera, no modem at all
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()

	// `-m 0` targets the first modem ModemManager knows about. On a
	// SIM7600G-H Pi this is always 0; multi-modem setups aren't a
	// thing for our hardware.
	out, err := exec.CommandContext(ctx, "mmcli", "-m", "0").Output()
	if err != nil {
		slog.Debug("mmcli failed reading IMSI", "err", err)
		return ""
	}

	// Match `imsi: <digits>` allowing for whitespace/punctuation around.
	// 14-15 digits is the IMSI spec range; tolerate a wider band so an
	// unusual format doesn't drop the value.
	re := regexp.MustCompile(`(?i)imsi:\s*([0-9]{10,16})`)
	m := re.FindSubmatch(out)
	if len(m) < 2 {
		return ""
	}
	imsi := strings.TrimSpace(string(m[1]))
	slog.Info("read sim_imsi from mmcli", "imsi_len", len(imsi))
	return imsi
}
