package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// SignRequest sets the Authorization header on an HTTP request using an
// ed25519 signature over METHOD\nPATH\nTIMESTAMP\nDEVICE_ID. The server
// verifies the signature against the camera's registered public key and
// rejects timestamps more than 5 minutes stale.
func SignRequest(req *http.Request, deviceID string, privateKey ed25519.PrivateKey) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	message := fmt.Sprintf("%s\n%s\n%s\n%s", req.Method, req.URL.Path, ts, deviceID)
	sig := ed25519.Sign(privateKey, []byte(message))
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	req.Header.Set("Authorization",
		fmt.Sprintf("Signature device_id=%s,ts=%s,sig=%s", deviceID, ts, sigB64))
}

// SignWHIPBearer returns the WHIP-specific transport encoding of the same
// ed25519 signature: <device_id>.<ts>.<sig_b64>. The dot-separated form
// fits inside `Authorization: Bearer …` (ffmpeg's `-authorization` flag
// prefixes "Bearer " and provides no way to escape `=` or `,` in the
// bearer value, so the camera-server WHIP exchange uses this encoding
// instead of the comma-separated Signature scheme).
//
// path is the URL path the WHIP POST will hit, e.g.
// "/api/v1/whip/<device_id>". Same canonical message
// ("POST\n<path>\n<ts>\n<device_id>"), same 5-minute skew window
// enforced server-side by auth.VerifySignature.
func SignWHIPBearer(path, deviceID string, privateKey ed25519.PrivateKey) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	message := fmt.Sprintf("POST\n%s\n%s\n%s", path, ts, deviceID)
	sig := ed25519.Sign(privateKey, []byte(message))
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	return fmt.Sprintf("%s.%s.%s", deviceID, ts, sigB64)
}
