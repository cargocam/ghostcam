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
