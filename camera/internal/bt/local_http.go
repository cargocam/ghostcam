package bt

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cargocam/ghostcam/common"
)

// ScanLocalHTTP runs a local, fully-offline provisioning HTTP server and
// returns the first valid onboarding payload submitted to it, mirroring
// the (nil, nil) "nothing here" contract of ScanQR / ScanBT so it slots
// into raceQRandBT unchanged.
//
// This is the shared downstream for the USB-gadget and SoftAP onboarding
// channels (milestone: Local-First Onboarding). The daemon is otherwise
// outbound-only (camera/client.go); this is the one inbound listener, and
// it exists only during the provisioning window.
//
// Plain HTTP is deliberate, but the justification is per-channel and must
// stay distinct:
//   - USB gadget: the listener binds to the point-to-point usb0 link
//     (10.55.0.1) — one physical wire, no radio, nothing to eavesdrop.
//   - SoftAP: the listener binds to the AP interface, whose confidentiality
//     comes from WPA2 link-layer encryption.
//
// It binds to the exact host:port given (never 0.0.0.0 in production) so
// the page is unreachable over WiFi/cellular. An empty addr disables the
// server: it returns (nil, nil) immediately, which is how every build
// without a configured gadget/AP link opts out.
func ScanLocalHTTP(ctx context.Context, deviceID, addr string) (*common.QRPayload, error) {
	if addr == "" {
		return nil, nil
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Non-fatal: on a device where the gadget/AP interface isn't up
		// (or the addr is already taken) we simply contribute nothing to
		// the race, exactly like the QR/BT stubs on unsupported hardware.
		slog.Warn("local provisioning server: listen failed, skipping channel", "addr", addr, "err", err)
		return nil, nil
	}

	// Buffered so the POST handler never blocks delivering the payload
	// even if we're mid-shutdown.
	payloadCh := make(chan *common.QRPayload, 1)

	prefix := deviceID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/provision", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		payload, perr := parseProvisionSubmission(r)
		if perr != nil {
			slog.Info("local provisioning server: rejected submission", "err", perr)
			w.WriteHeader(http.StatusBadRequest)
			renderPage(w, prefix, perr.Error())
			return
		}
		// Non-blocking send: first valid submission wins, later ones are
		// dropped (the race is already being torn down).
		select {
		case payloadCh <- payload:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(successHTML))
	})
	// Everything else (including "/") shows the form. Serving the form on
	// any path also makes the mass-storage redirect + captive-portal
	// fallbacks land somewhere useful regardless of the exact URL.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		renderPage(w, prefix, "")
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	slog.Info("local provisioning server listening", "addr", addr, "device", prefix)

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Shutdown drains in-flight requests, so the success page finishes
		// flushing to the browser before the listener closes.
		_ = srv.Shutdown(shutdownCtx)
	}()

	select {
	case payload := <-payloadCh:
		slog.Info("local provisioning server: payload accepted", "device", prefix)
		return payload, nil
	case <-ctx.Done():
		// Race lost to QR/BT (or timed out). context.Canceled is filtered
		// by raceQRandBT, so this doesn't mask a real error.
		return nil, ctx.Err()
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil, nil
		}
		return nil, err
	}
}

// parseProvisionSubmission accepts either an HTML form POST (the no-JS
// path that works on every browser, iOS Safari included) or a JSON
// common.QRPayload body (programmatic / future JS clients). Both funnel
// into the identical common.QRPayload the QR and BT channels produce, so
// there are no parallel provisioning types.
func parseProvisionSubmission(r *http.Request) (*common.QRPayload, error) {
	var p common.QRPayload

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<10)).Decode(&p); err != nil {
			return nil, errors.New("could not read submission")
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return nil, errors.New("could not read submission")
		}
		p.Server = strings.TrimSpace(r.PostFormValue("server"))
		p.Token = strings.TrimSpace(r.PostFormValue("token"))
		p.WifiSSID = strings.TrimSpace(r.PostFormValue("wifi_ssid"))
		p.WifiPassword = r.PostFormValue("wifi_password")
	}

	// Same validation the BT/QR channels apply before accepting a payload.
	if p.Server == "" || p.Token == "" {
		return nil, errors.New("server URL and provision token are both required")
	}
	return &p, nil
}

func renderPage(w http.ResponseWriter, devicePrefix, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = provisionPageTmpl.Execute(w, struct {
		Device string
		Error  string
	}{Device: devicePrefix, Error: errMsg})
}

// provisionPageTmpl is intentionally a single self-contained document with
// no external assets — it has to render over a USB cable or SoftAP with no
// internet in the loop. html/template escapes the device id and any error.
var provisionPageTmpl = template.Must(template.New("provision").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Set up Ghostcam-{{.Device}}</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
         max-width: 30rem; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
  h1 { font-size: 1.4rem; }
  label { display: block; margin-top: 1rem; font-weight: 600; font-size: .9rem; }
  input { width: 100%; padding: .55rem; margin-top: .25rem; font-size: 1rem;
          border: 1px solid #8888; border-radius: .4rem; box-sizing: border-box; }
  button { margin-top: 1.5rem; width: 100%; padding: .7rem; font-size: 1rem;
           border: 0; border-radius: .4rem; background: #2563eb; color: #fff; font-weight: 600; }
  .hint { color: #6b7280; font-size: .85rem; }
  .err { background: #fee2e2; color: #991b1b; padding: .6rem .8rem; border-radius: .4rem; margin-top: 1rem; }
  code { background: #8881; padding: .1rem .3rem; border-radius: .25rem; }
</style>
</head>
<body>
<h1>Set up Ghostcam-{{.Device}}</h1>
<p class="hint">This page is served by the camera over its local link — no internet
needed here. Generate a provision token in the Ghostcam web app's
<strong>Get Started</strong> card, then paste it below.</p>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<form method="POST" action="/provision">
  <label>Server URL
    <input name="server" type="url" inputmode="url" placeholder="https://ghostcam.fly.dev"
           value="https://ghostcam.fly.dev" required>
  </label>
  <label>Provision token
    <input name="token" type="text" autocomplete="off" autocapitalize="off"
           spellcheck="false" placeholder="paste the token from Get Started" required>
  </label>
  <label>WiFi network <span class="hint">(leave blank if using cellular)</span>
    <input name="wifi_ssid" type="text" autocapitalize="off" placeholder="SSID">
  </label>
  <label>WiFi password
    <input name="wifi_password" type="password" placeholder="Password">
  </label>
  <button type="submit">Connect camera</button>
</form>
</body>
</html>`))

const successHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Ghostcam connecting</title>
<style>body{font-family:system-ui,-apple-system,sans-serif;max-width:30rem;margin:3rem auto;padding:0 1rem;line-height:1.5;text-align:center}h1{font-size:1.4rem}</style>
</head><body>
<h1>Camera is connecting…</h1>
<p>Credentials received. The camera is applying WiFi (if provided) and
enrolling with the server. It will appear in your Ghostcam dashboard
shortly — you can unplug the cable once it shows up.</p>
</body></html>`
