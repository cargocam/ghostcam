// sigverify is a stdin-driven Go harness that proves the Python camera's
// ed25519 signing produces bytes the server can verify. It exists for
// tests/test_signing_roundtrip.py — Python pipes a JSON object describing
// {seed, method, path, ts, device_id, signature} and this binary
// reconstructs the canonical message and verifies the signature using
// the same crypto/ed25519 + base64.RawURLEncoding the server uses.
//
// Two modes:
//   sign   — read {seed, method, path, ts, device_id} from stdin, write
//            the canonical Go-side signature (b64 RawURL) to stdout.
//   verify — read {seed, method, path, ts, device_id, signature} from
//            stdin, write "ok" or "fail: <reason>" to stdout.
//
// The "sign" mode lets Python tests assert byte-equality without
// rerunning a Go fixture command — the test handles the comparison.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type request struct {
	Seed      string `json:"seed"`       // hex (64 chars)
	Method    string `json:"method"`
	Path      string `json:"path"`
	TS        int64  `json:"ts"`
	DeviceID  string `json:"device_id"`
	Signature string `json:"signature,omitempty"` // b64 RawURL, only for verify
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "sign" && os.Args[1] != "verify") {
		fmt.Fprintln(os.Stderr, "usage: sigverify (sign|verify)")
		os.Exit(2)
	}
	mode := os.Args[1]

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read stdin:", err)
		os.Exit(1)
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		fmt.Fprintln(os.Stderr, "json:", err)
		os.Exit(1)
	}

	seed, err := hex.DecodeString(req.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		fmt.Fprintln(os.Stderr, "bad seed")
		os.Exit(1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	msg := []byte(fmt.Sprintf("%s\n%s\n%d\n%s", req.Method, req.Path, req.TS, req.DeviceID))

	switch mode {
	case "sign":
		sig := ed25519.Sign(priv, msg)
		fmt.Println(base64.RawURLEncoding.EncodeToString(sig))
	case "verify":
		sig, err := base64.RawURLEncoding.DecodeString(req.Signature)
		if err != nil {
			fmt.Println("fail: decode signature:", err)
			os.Exit(0)
		}
		if !ed25519.Verify(pub, msg, sig) {
			fmt.Println("fail: signature did not verify")
			os.Exit(0)
		}
		fmt.Println("ok")
	}
}
