package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/common"
	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/auth"
)

// EnrollmentQR handles GET/POST /api/v1/cameras/enroll/qr.
// Returns JSON with the QR payload string for client-side QR rendering.
func (a *App) EnrollmentQR(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body apitypes.QRRequest
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
	tokenHash := auth.HMACToken(rawToken, a.HMACSecret)

	now := time.Now().Unix()
	expiresAt := now + int64(ttlHours*3600)

	if err := a.DB.CreateProvisionToken(r.Context(), tokenHash, userID, expiresAt); err != nil {
		slog.Error("enrollment qr: create provision token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	serverURL := a.Config.PublicURL
	if serverURL == "" {
		serverURL = fmt.Sprintf("https://%s", r.Host)
	}
	payload := common.QRPayload{
		Server:       serverURL,
		Token:        rawToken,
		WifiSSID:     body.WifiSSID,
		WifiPassword: body.WifiPassword,
	}
	// Only the password is dropped when the SSID is empty — WifiPassword on
	// its own is meaningless. common.QRPayload's omitempty tags handle the
	// rest.
	if payload.WifiSSID == "" {
		payload.WifiPassword = ""
	}

	payloadBytes, _ := json.Marshal(payload)

	slog.Info("audit", "event_type", "enrollment_started", "user_id", userID)

	writeJSON(w, http.StatusOK, apitypes.QRResponse{
		Payload:   string(payloadBytes),
		Token:     rawToken,
		ExpiresAt: expiresAt,
	})
}
