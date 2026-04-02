package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/server/auth"
	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/db"
	qrcode "github.com/skip2/go-qrcode"
)

type qrRequest struct {
	WifiSSID     string `json:"wifi_ssid,omitempty"`
	WifiPassword string `json:"wifi_password,omitempty"`
	TTLHours     uint64 `json:"ttl_hours,omitempty"`
}

// EnrollmentQR handles GET/POST /api/v1/cameras/enroll/qr.
func (h *Handlers) EnrollmentQR(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)

	var body qrRequest
	if r.Method == http.MethodPost && r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	ttlHours := body.TTLHours
	if ttlHours == 0 {
		ttlHours = 24
	}
	if ttlHours > 168 {
		ttlHours = 168
	}

	rawToken := auth.GenerateRandomPassword()
	tokenHash := auth.HMACToken(rawToken, h.HMACSecret)

	now := time.Now().Unix()
	expiresAt := now + int64(ttlHours*3600)

	if err := h.DB.CreateProvisionToken(r.Context(), tokenHash, userID, expiresAt); err != nil {
		slog.Error("enrollment qr: create provision token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Build QR payload — use configured public URL, fall back to request Host
	serverURL := h.PublicURL
	if serverURL == "" {
		serverURL = fmt.Sprintf("https://%s", r.Host)
	}
	payload := map[string]string{
		"s": serverURL,
		"t": rawToken,
	}
	if body.WifiSSID != "" {
		payload["w"] = body.WifiSSID
		if body.WifiPassword != "" {
			payload["p"] = body.WifiPassword
		}
	}

	payloadBytes, _ := json.Marshal(payload)

	png, err := qrcode.Encode(string(payloadBytes), qrcode.Medium, 256)
	if err != nil {
		slog.Error("enrollment qr: generate QR failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	db.AuditLog("enrollment_started", "user_id", userID)

	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	w.Write(png)
}
