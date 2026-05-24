package commands

// Typed "unstick" actions from ghostcam#119. Each is a fixed argv —
// no operator-supplied input, no string allowlist, no escape hatch.
// New actions are added as discrete command types, not as parameters
// to a generic exec.

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// restartActionTimeout bounds the systemctl call. Restart of
// NetworkManager / ModemManager normally completes in well under 10 s;
// if it doesn't, the camera should keep going — a longer wait blocks
// the telemetry-poll command-dispatch goroutine but not the main
// capture/upload loops, so the hit is minor.
const restartActionTimeout = 30 * time.Second

// RestartGhostcamService is the dispatched body of restart_service.
// Same pattern as the existing "reboot" command: just os.Exit cleanly
// and let systemd's Restart=always policy respawn us. Calling
// `systemctl restart ghostcam-camera` from within ourselves would be
// equivalent but indirect — systemd would TERM us anyway. The
// telemetry-poll loop has already acked the command via the next poll
// it will fire from the restarted instance, so no risk of an
// infinite "restart on every poll" loop.
func RestartGhostcamService() {
	slog.Info("restart_service: exiting for systemd-managed restart")
	os.Exit(0)
}

// RestartModemManager runs `systemctl restart ModemManager`. Blocks
// until systemctl returns or the 30 s timeout fires. mmcli queries
// from CaptureDiagBundle may briefly fail after restart while the
// daemon comes back up; that's a property of how ModemManager works,
// not a bug to work around here.
func RestartModemManager(ctx context.Context) {
	slog.Info("restart_modem_manager: invoking systemctl restart ModemManager")
	cctx, cancel := context.WithTimeout(ctx, restartActionTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "systemctl", "restart", "ModemManager").CombinedOutput()
	if err != nil {
		slog.Error("restart_modem_manager failed",
			"err", err, "output", string(out))
		return
	}
	slog.Info("restart_modem_manager: systemctl returned")
}

// RestartNetworkManager runs `systemctl restart NetworkManager`. Same
// shape as RestartModemManager. NM restart drops the default route
// for a few seconds while interfaces re-register; this can cause one
// telemetry-poll cycle to fail, but the next succeeds.
func RestartNetworkManager(ctx context.Context) {
	slog.Info("restart_network_manager: invoking systemctl restart NetworkManager")
	cctx, cancel := context.WithTimeout(ctx, restartActionTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "systemctl", "restart", "NetworkManager").CombinedOutput()
	if err != nil {
		slog.Error("restart_network_manager failed",
			"err", err, "output", string(out))
		return
	}
	slog.Info("restart_network_manager: systemctl returned")
}
