package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/cargocam/ghostcam/common"
	"github.com/cargocam/ghostcam/server/auth"
	"github.com/google/uuid"
)

// Provision handles POST /api/v1/cameras/provision.
func (a *App) Provision(w http.ResponseWriter, r *http.Request) {
	var body common.ProvisionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Token == "" || body.DeviceSerial == "" {
		writeError(w, http.StatusBadRequest, "token and device_serial required")
		return
	}

	tokenHash := auth.HMACToken(body.Token, a.HMACSecret)

	userID, err := a.DB.ClaimProvisionToken(r.Context(), tokenHash)
	if err != nil {
		slog.Error("provision: claim token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if userID == nil {
		writeError(w, http.StatusUnauthorized, "invalid or expired provisioning token")
		return
	}

	// Check if this device serial is already provisioned (re-provisioning).
	existing, err := a.DB.GetCameraBySerial(r.Context(), body.DeviceSerial)
	if err != nil {
		slog.Error("provision: check existing failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	var deviceID string
	if existing != nil {
		deviceID = existing.DeviceID
		slog.Info("re-provisioning existing camera", "device_id", deviceID, "serial", body.DeviceSerial)
	} else {
		deviceID = uuid.New().String()
		if err := a.DB.CreateProvisionedCamera(r.Context(), deviceID, *userID, body.DeviceSerial); err != nil {
			slog.Error("provision: create camera failed", "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
	}

	apiKey := auth.GenerateRandomPassword()
	apiKeyHash := auth.HMACToken(apiKey, a.HMACSecret)

	// Delete old API key if re-provisioning, then create new.
	_ = a.DB.DeleteCameraAPIKey(r.Context(), deviceID)
	if err := a.DB.CreateCameraAPIKey(r.Context(), deviceID, apiKeyHash); err != nil {
		slog.Error("provision: create api key failed", "error", err)
		if existing == nil {
			_ = a.DB.DeleteCamera(r.Context(), deviceID)
		}
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "camera_provisioned", "device_id", deviceID, "user_id", *userID, "device_serial", body.DeviceSerial)

	writeJSON(w, http.StatusOK, common.ProvisionResponse{
		APIKey:   apiKey,
		DeviceID: deviceID,
	})
}
