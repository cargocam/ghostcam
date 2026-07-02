package bt

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cargocam/ghostcam/common"
)

// freeAddr grabs an ephemeral loopback port and releases it so
// ScanLocalHTTP can bind it. There's a tiny reuse window, but it's the
// standard trick for "give me a bindable address" in tests.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// waitReady polls the form endpoint until the server is accepting
// connections so the test doesn't race the goroutine's Listen.
func waitReady(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		resp, err := http.Get("http://" + addr + "/")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never became ready")
}

func TestScanLocalHTTP_FormSubmission(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type res struct {
		p   *common.QRPayload
		err error
	}
	done := make(chan res, 1)
	go func() {
		p, err := ScanLocalHTTP(ctx, "45a8b310deadbeef", addr)
		done <- res{p, err}
	}()
	waitReady(t, addr)

	form := url.Values{
		"server":        {"https://example.test"},
		"token":         {"tok-123"},
		"wifi_ssid":     {"lab-wifi"},
		"wifi_password": {"hunter2"},
	}
	resp, err := http.PostForm("http://"+addr+"/provision", form)
	if err != nil {
		t.Fatalf("post form: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	r := <-done
	if r.err != nil {
		t.Fatalf("ScanLocalHTTP err: %v", r.err)
	}
	if r.p == nil {
		t.Fatal("expected a payload, got nil")
	}
	if r.p.Server != "https://example.test" || r.p.Token != "tok-123" {
		t.Errorf("payload server/token = %q/%q", r.p.Server, r.p.Token)
	}
	if r.p.WifiSSID != "lab-wifi" || r.p.WifiPassword != "hunter2" {
		t.Errorf("payload wifi = %q/%q", r.p.WifiSSID, r.p.WifiPassword)
	}
}

func TestScanLocalHTTP_JSONSubmission(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan *common.QRPayload, 1)
	go func() {
		p, _ := ScanLocalHTTP(ctx, "dev", addr)
		done <- p
	}()
	waitReady(t, addr)

	body, _ := json.Marshal(common.QRPayload{Server: "https://s.test", Token: "jtok"})
	resp, err := http.Post("http://"+addr+"/provision", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post json: %v", err)
	}
	_ = resp.Body.Close()

	p := <-done
	if p == nil || p.Token != "jtok" {
		t.Fatalf("json submission not accepted: %+v", p)
	}
}

func TestScanLocalHTTP_RejectsMissingFields(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _, _ = ScanLocalHTTP(ctx, "dev", addr) }()
	waitReady(t, addr)

	// Missing token → 400, and the server keeps waiting (doesn't return).
	resp, err := http.PostForm("http://"+addr+"/provision", url.Values{"server": {"https://s.test"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// Form should still be reachable (server not torn down by a bad submit).
	if r2, err := http.Get("http://" + addr + "/"); err == nil {
		_ = r2.Body.Close()
	} else {
		t.Fatalf("form gone after bad submit: %v", err)
	}
}

func TestScanLocalHTTP_EmptyAddrDisabled(t *testing.T) {
	p, err := ScanLocalHTTP(context.Background(), "dev", "")
	if p != nil || err != nil {
		t.Fatalf("empty addr should be a no-op, got (%v, %v)", p, err)
	}
}

func TestScanLocalHTTP_ContextCancel(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := ScanLocalHTTP(ctx, "dev", addr)
		done <- err
	}()
	waitReady(t, addr)
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("want context canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ScanLocalHTTP did not return after cancel")
	}
}
