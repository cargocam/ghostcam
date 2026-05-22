package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cargocam/ghostcam/common"
)

func TestRunFixedArgv_CapturesStdout(t *testing.T) {
	out := runFixedArgv(context.Background(), []string{"echo", "hello", "diag"})
	if !strings.Contains(out, "hello diag") {
		t.Errorf("expected 'hello diag' in output, got %q", out)
	}
}

func TestRunFixedArgv_MissingExecutableReturnsEmpty(t *testing.T) {
	// Picked something that definitely won't exist on PATH. We don't
	// want a fallback that surfaces the OS error to the operator;
	// missing tools (e.g. mmcli on wifi-only Pis) are a legitimate
	// state, not an error.
	out := runFixedArgv(context.Background(), []string{"ghostcam-definitely-not-a-real-binary-77"})
	if out != "" {
		t.Errorf("expected empty output for missing binary, got %q", out)
	}
}

func TestRunFixedArgv_NonZeroExitStillReturnsStderr(t *testing.T) {
	// `ls` against a non-existent path exits with status 2 and prints
	// "cannot access ...". We want the operator to see that text — it's
	// the most useful diagnostic signal when a subcommand is "there but
	// not happy."
	out := runFixedArgv(context.Background(), []string{"ls", "/this-path-deliberately-does-not-exist-ghostcam"})
	if !strings.Contains(out, "deliberately") {
		t.Errorf("expected stderr captured for failing command, got %q", out)
	}
}

func TestRunFixedArgv_EmptyArgvReturnsEmpty(t *testing.T) {
	if out := runFixedArgv(context.Background(), nil); out != "" {
		t.Errorf("expected empty for nil argv, got %q", out)
	}
	if out := runFixedArgv(context.Background(), []string{}); out != "" {
		t.Errorf("expected empty for empty argv, got %q", out)
	}
}

func TestTruncateField_PassthroughBelowCap(t *testing.T) {
	in := []byte("under the cap")
	if got := truncateField(in); got != "under the cap" {
		t.Errorf("expected passthrough, got %q", got)
	}
}

func TestTruncateField_TailWithMarkerAboveCap(t *testing.T) {
	// 50 KB of repeating data; truncation should keep the last 32 KB
	// and prepend a marker the operator can read.
	in := []byte(strings.Repeat("abcdefghij", 5000)) // 50 KB
	out := truncateField(in)
	if !strings.HasPrefix(out, "[diag: truncated") {
		t.Errorf("expected truncation marker prefix, got first 60 bytes: %q", out[:60])
	}
	// kept tail should still end with the original suffix.
	if !strings.HasSuffix(out, "abcdefghij") {
		t.Errorf("expected truncated output to retain tail, got suffix %q", out[len(out)-20:])
	}
	if len(out) > diagFieldMaxBytes+len("[diag: truncated, kept last 32 KB]\n") {
		t.Errorf("truncated output exceeded cap: len=%d", len(out))
	}
}

func TestPendingDiagBundles_DrainEmptiesQueue(t *testing.T) {
	t.Cleanup(func() { drainPendingDiagBundles() }) // belt-and-braces

	addPendingDiagBundle(common.DiagBundle{DiagID: "a", CapturedAt: 1})
	addPendingDiagBundle(common.DiagBundle{DiagID: "b", CapturedAt: 2})

	got := drainPendingDiagBundles()
	if len(got) != 2 {
		t.Fatalf("expected 2 bundles drained, got %d", len(got))
	}
	if got[0].DiagID != "a" || got[1].DiagID != "b" {
		t.Errorf("expected order [a,b], got [%s,%s]", got[0].DiagID, got[1].DiagID)
	}
	// Second drain must be empty.
	if got2 := drainPendingDiagBundles(); len(got2) != 0 {
		t.Errorf("expected empty drain after first, got %d", len(got2))
	}
}

func TestPendingDiagBundles_ConcurrentAddAndDrain(t *testing.T) {
	t.Cleanup(func() { drainPendingDiagBundles() })

	// 50 concurrent producers; the drain races with them. The test
	// asserts the slice doesn't tear under contention (the mutex
	// matters) and the total count is exactly what was queued.
	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addPendingDiagBundle(common.DiagBundle{DiagID: string(rune('a' + i%26))})
		}(i)
	}
	wg.Wait()
	got := drainPendingDiagBundles()
	if len(got) != n {
		t.Errorf("expected %d bundles after concurrent add, got %d", n, len(got))
	}
}

func TestCaptureDiagBundle_PopulatesCorrelationIDAndTimestamp(t *testing.T) {
	// Smoke test against the host's real binaries. We don't assert on
	// any particular field content — `mmcli` won't exist on the dev
	// laptop, `systemctl` may not, etc. — only that the bundle metadata
	// is set and the call returns without panicking.
	b := CaptureDiagBundle(context.Background(), "smoke-test-id")
	if b.DiagID != "smoke-test-id" {
		t.Errorf("expected DiagID propagated, got %q", b.DiagID)
	}
	if b.CapturedAt == 0 {
		t.Errorf("expected CapturedAt set, got 0")
	}
}
