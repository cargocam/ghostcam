//go:build linux && !synthetic

package diag

import (
	"context"
	"os"
	"strings"
	"syscall"
	"time"
)

// clbsPort is the spare AT port on the SIM7600. ModemManager drives the
// primary AT port (ttyUSB2); ttyUSB3 is normally free, and the udev rule
// ships it MODE=0666 so the non-root daemon can open it.
const clbsPort = "/dev/ttyUSB3"

// probeCLBS sends SIMCom's `AT+CLBS` location query on the spare AT port
// and returns the raw exchange, to validate whether the module can return
// a coarse (cell-tower) lat/lon for free via SIMCom's LBS backend — no
// API key. Diagnostic only: best-effort, bounded, never fatal. A real
// success looks like `+CLBS: 0,<lon>,<lat>,<acc>`; a failure is a
// non-zero location code (e.g. `+CLBS: 4`) or silence.
func probeCLBS(ctx context.Context) string {
	// O_NONBLOCK so the fd is runtime-poller managed and SetReadDeadline
	// works; NOCTTY so opening the tty doesn't make it our controlling
	// terminal. USB CDC ignores baud, so no termios setup is needed for
	// line-based AT.
	f, err := os.OpenFile(clbsPort, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "open " + clbsPort + " failed: " + err.Error() +
			" (ModemManager may own it, or the port doesn't exist)"
	}
	defer f.Close()

	// Sanity ping, then the LBS query. CLBS reaches SIMCom's server over
	// the data bearer, so it can take several seconds.
	_, _ = f.Write([]byte("AT\r"))
	time.Sleep(300 * time.Millisecond)
	_, _ = f.Write([]byte("AT+CLBS=1,1\r"))

	var out strings.Builder
	buf := make([]byte, 512)
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return out.String()
		default:
		}
		_ = f.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, rerr := f.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if rerr != nil {
			if os.IsTimeout(rerr) {
				// Stop early once the CLBS reply line has arrived.
				if strings.Contains(out.String(), "+CLBS:") {
					break
				}
				continue
			}
			break
		}
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		return "no response on " + clbsPort + " (port silent or owned by ModemManager)"
	}
	return s
}
