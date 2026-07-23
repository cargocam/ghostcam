package diag

// Rescue/diagnostic capture (see ghostcam#119). Each field of DiagBundle
// is produced by running a fixed argv — no operator input, no shell, no
// allowlist to maintain. Missing subcommands (e.g. mmcli on a wifi-only
// Pi) or non-zero exits leave the field empty rather than failing the
// whole bundle. Captures run in parallel with a per-field 5 s timeout so
// one stuck subprocess can't poison the rest.

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/cargocam/ghostcam/common"
)

// pendingDiagBundles holds captured DiagBundles waiting to ride out on
// the next telemetry poll. Mutex-guarded so HandleCommand's goroutine
// and PostTelemetry's drain can't race.
var (
	pendingDiagBundlesMu sync.Mutex
	pendingDiagBundles   []common.DiagBundle
)

const (
	// diagFieldTimeout bounds a single subcommand. 5s is generous for
	// the heaviest field (journalctl --since=1h) on a healthy Pi and
	// short enough that a wedged subprocess doesn't delay the whole
	// bundle ship by more than a handful of seconds (the captures run
	// in parallel, so total wall time is one timeout, not sum).
	diagFieldTimeout = 5 * time.Second
	// diagFieldMaxBytes is the per-field output cap. journalctl and
	// dmesg can produce orders of magnitude more than this; we keep
	// the tail (most recent output, where the relevant signal usually
	// is) and prepend a truncation marker so the operator knows.
	diagFieldMaxBytes = 32 * 1024
)

// AddPendingDiagBundle appends a finished bundle to the pending slice.
// Called from the diag_bundle capture goroutine in internal/commands.
func AddPendingDiagBundle(b common.DiagBundle) {
	pendingDiagBundlesMu.Lock()
	pendingDiagBundles = append(pendingDiagBundles, b)
	pendingDiagBundlesMu.Unlock()
}

// DrainPendingDiagBundles returns and clears any captured bundles.
// PostTelemetry (called from camera/client.go) calls this when
// assembling the request body. On a 2xx response the caller can treat
// the drain as authoritative; on failure the caller is responsible
// for re-enqueueing via AddPendingDiagBundle so a transient network
// blip doesn't lose the bundle. (For #119 we accept that loss:
// bundles are explicit operator requests; if the poll fails the
// operator can just issue another diag_bundle command.)
func DrainPendingDiagBundles() []common.DiagBundle {
	pendingDiagBundlesMu.Lock()
	defer pendingDiagBundlesMu.Unlock()
	if len(pendingDiagBundles) == 0 {
		return nil
	}
	out := pendingDiagBundles
	pendingDiagBundles = nil
	return out
}

// CaptureDiagBundle runs every fixed-argv subcommand in parallel and
// returns a populated DiagBundle. Caller chooses whether to enqueue
// the result via addPendingDiagBundle.
func CaptureDiagBundle(ctx context.Context, diagID string) common.DiagBundle {
	type capture struct {
		field *string
		argv  []string
	}
	bundle := common.DiagBundle{
		DiagID:     diagID,
		CapturedAt: time.Now().UnixMilli(),
	}
	captures := []capture{
		{&bundle.ModemList, []string{"mmcli", "-L"}},
		{&bundle.ModemDetail, []string{"mmcli", "-m", "0"}},
		{&bundle.NMConnections, []string{"nmcli", "-t", "-f", "NAME,UUID,TYPE,DEVICE", "con", "show"}},
		{&bundle.NMDevices, []string{"nmcli", "-t", "-f", "DEVICE,TYPE,STATE,CONNECTION", "dev", "status"}},
		{&bundle.IPAddr, []string{"ip", "-4", "-o", "addr"}},
		{&bundle.IPRoute, []string{"ip", "route"}},
		{&bundle.ServiceStatus, []string{"systemctl", "--no-pager", "status", "ghostcam-camera"}},
		{&bundle.ServiceLogs, []string{"journalctl", "--no-pager", "-u", "ghostcam-camera", "--since=1 hour ago"}},
		{&bundle.Dmesg, []string{"journalctl", "--no-pager", "-k", "--since=1 hour ago"}},
		{&bundle.Disk, []string{"df", "-h"}},
		{&bundle.Mem, []string{"free", "-m"}},
		{&bundle.Uptime, []string{"uptime"}},
		{&bundle.PkgVersion, []string{"dpkg-query", "-W", "ghostcam-camera"}},
	}

	var wg sync.WaitGroup
	for _, c := range captures {
		wg.Add(1)
		go func(c capture) {
			defer wg.Done()
			*c.field = runFixedArgv(ctx, c.argv)
		}(c)
	}

	// SIMCom AT+CLBS coarse-location probe (raw serial on the spare AT
	// port — not a fixed-argv subcommand). Runs concurrently; result is
	// appended to ModemDetail after the wait so we don't add a contract
	// field (and force a server module bump) for a validation probe.
	var clbs string
	wg.Add(1)
	go func() {
		defer wg.Done()
		clbs = probeCLBS(ctx)
	}()

	// mmcli --location-get: the 3GPP serving-cell block the coarse-location
	// fallback depends on. Captured here (as the non-root daemon, so it
	// reflects exactly what the daemon can read) to diagnose why telemetry
	// isn't reporting cell_op — empty output means 3GPP location isn't
	// enabled or isn't readable without privilege. Appended to ModemDetail
	// to avoid a DiagBundle contract field.
	// Power / throttling — the definitive remote signal for the modem
	// brownouts: vcgencmd get_throttled bit 0 = under-voltage now, bit 16 =
	// under-voltage occurred since boot. Usable by the ghostcam user
	// (member of the `video` group). Appended to ModemDetail since the
	// power question is what's resetting the modem.
	var throttled, volts string
	var locGet, locStatus, gpsPipe string
	wg.Add(1)
	go func() {
		defer wg.Done()
		throttled = runFixedArgv(ctx, []string{"vcgencmd", "get_throttled"})
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		volts = runFixedArgv(ctx, []string{"vcgencmd", "measure_volts"})
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		locGet = runFixedArgv(ctx, []string{"mmcli", "-m", "0", "--location-get"})
	}()
	// GPS diagnostics so a stuck GNSS is debuggable remotely (no SSH):
	// --location-status shows which location sources are enabled;
	// gpspipe -w shows whether gpsd has a device attached and is
	// receiving fixes (the DEVICES / TPV messages). Both read-only.
	wg.Add(1)
	go func() {
		defer wg.Done()
		locStatus = runFixedArgv(ctx, []string{"mmcli", "-m", "0", "--location-status"})
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		gpsPipe = runFixedArgv(ctx, []string{"gpspipe", "-w", "-n", "5"})
	}()

	wg.Wait()
	if throttled != "" || volts != "" {
		bundle.ModemDetail += "\n\n=== power: vcgencmd get_throttled / measure_volts ===\n" + throttled + volts
	}
	if locStatus != "" {
		bundle.ModemDetail += "\n\n=== mmcli -m 0 --location-status ===\n" + locStatus
	}
	if locGet != "" {
		bundle.ModemDetail += "\n\n=== mmcli -m 0 --location-get ===\n" + locGet
	}
	if gpsPipe != "" {
		bundle.ModemDetail += "\n\n=== gpspipe -w -n 5 (gpsd devices + fixes) ===\n" + gpsPipe
	}
	if clbs != "" {
		bundle.ModemDetail += "\n\n=== AT+CLBS probe (ttyUSB3, SIMCom LBS) ===\n" + clbs
	}
	return bundle
}

// devExecTimeout / devExecMaxBytes bound a dev_exec command.
const (
	devExecTimeout  = 30 * time.Second
	devExecMaxBytes = 128 * 1024
)

// RunDevExec runs an arbitrary shell command (DEV-ONLY remote exec, see
// CameraCommand.Command) as the daemon user and returns its combined
// stdout+stderr wrapped in a DiagBundle so it rides back over the
// existing diag channel. Bounded timeout + output cap. Non-root (the
// daemon's own privileges) — not a root shell. REMOVE before a fleet.
func RunDevExec(ctx context.Context, diagID, command string) common.DiagBundle {
	b := common.DiagBundle{DiagID: diagID, CapturedAt: time.Now().UnixMilli()}
	cctx, cancel := context.WithTimeout(ctx, devExecTimeout)
	defer cancel()
	out, _ := exec.CommandContext(cctx, "sh", "-c", command).CombinedOutput()
	text := string(out)
	if len(text) > devExecMaxBytes {
		text = "[truncated, kept last 128 KB]\n" + text[len(text)-devExecMaxBytes:]
	}
	suffix := ""
	if cctx.Err() == context.DeadlineExceeded {
		suffix = "\n[dev_exec: timed out after " + devExecTimeout.String() + "]"
	}
	b.Exec = "$ " + command + "\n" + text + suffix
	return b
}

// runFixedArgv runs the given argv with a bounded timeout, returns
// stdout+stderr concatenated (most diagnostic subcommands write
// useful context to stderr — mmcli error messages, systemctl exit
// reasons, etc.). Missing executable, timeout, or non-zero exit all
// just shape what the operator sees in the field — the result is
// returned to the server either way. Truncates to diagFieldMaxBytes.
//
// fixed-argv contract: callers MUST hard-code argv from string literals.
// runFixedArgv has no protection against shell metacharacters because
// it doesn't invoke a shell, but it also can't sanitize a caller-built
// string passed in as args[0]. Per #119 design: this function is only
// invoked from CaptureDiagBundle's local literal slices.
func runFixedArgv(ctx context.Context, argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, diagFieldTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	// Combine streams; many of these tools write context on stderr even
	// on success (mmcli "no modems available", systemctl "inactive").
	combined := stdout.Bytes()
	if stderr.Len() > 0 {
		if len(combined) > 0 {
			combined = append(combined, '\n')
		}
		combined = append(combined, stderr.Bytes()...)
	}

	if cctx.Err() == context.DeadlineExceeded {
		return prefixTruncated("[diag: timed out after "+diagFieldTimeout.String()+"]\n", combined)
	}
	if err != nil && len(combined) == 0 {
		// Most likely "executable not found" — leave the field empty.
		// The operator can read this from a sibling field's context (an
		// nmcli error implies NM isn't installed, which implies wifi
		// isn't managed by NM, etc.). We log so it's visible in
		// journalctl on the camera itself.
		slog.Debug("diag field empty", "argv", argv, "err", err)
		return ""
	}
	return truncateField(combined)
}

// truncateField caps a captured field at diagFieldMaxBytes, keeping the
// TAIL of the output (where the recent / relevant signal usually lives
// for journalctl + dmesg) and prepending a marker so the operator knows.
func truncateField(b []byte) string {
	if len(b) <= diagFieldMaxBytes {
		return string(b)
	}
	keep := b[len(b)-diagFieldMaxBytes:]
	return "[diag: truncated, kept last 32 KB]\n" + string(keep)
}

// prefixTruncated wraps a fixed prefix around a possibly-truncated tail.
func prefixTruncated(prefix string, b []byte) string {
	if len(b) > diagFieldMaxBytes {
		b = b[len(b)-diagFieldMaxBytes:]
	}
	return prefix + string(b)
}
