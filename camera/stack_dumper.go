package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"syscall"
	"time"
)

// runStackDumper writes a goroutine stack snapshot to <dataDir>/
// goroutines-<pid>-<unix-ms>.txt every time the process receives
// SIGUSR1. Blocking; intended to run as a long-lived goroutine.
//
// Designed around the cargocam/ghostcam#134 hang: the daemon parks
// every worker goroutine on a futex while systemd still reports
// active(running), so SIGQUIT-then-restart loses the diagnostic state.
// SIGUSR1 captures the state without disturbing the daemon, and the
// resulting file is small enough (~tens of KB) to scp off after
// reboot or include in a future diag_bundle without paging.
func runStackDumper(dataDir string) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	for range sig {
		if err := dumpStacks(dataDir); err != nil {
			slog.Warn("stack dump failed", "err", err)
		}
	}
}

func dumpStacks(dataDir string) error {
	name := fmt.Sprintf("goroutines-%d-%d.txt", os.Getpid(), time.Now().UnixMilli())
	path := filepath.Join(dataDir, name)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// debug=2 includes the full stack of every goroutine, not just a
	// summary count by state. That's what's needed to identify which
	// channel / mutex the worker pile-up is blocked on.
	if err := pprof.Lookup("goroutine").WriteTo(f, 2); err != nil {
		return err
	}
	slog.Info("goroutine stacks dumped (SIGUSR1)", "path", path)
	return nil
}
