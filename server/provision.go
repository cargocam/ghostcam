package main

import (
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/cargocam/ghostcam/common"
	"github.com/cargocam/ghostcam/server/auth"
)

// Provision handles POST /api/v1/cameras/provision.
//
// The camera sends its ed25519 public key for registration — like adding
// to SSH authorized_keys. The camera's device_id is derived from its
// public key (SHA-256 fingerprint), so it's stable across server switches.
func (a *App) Provision(w http.ResponseWriter, r *http.Request) {
	var body common.ProvisionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Token == "" || body.DeviceSerial == "" || body.PublicKey == "" || body.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "token, device_serial, public_key, and device_id required")
		return
	}

	// Validate public key format: 64 hex chars = 32 bytes (ed25519 public key).
	pubBytes, err := hex.DecodeString(body.PublicKey)
	if err != nil || len(pubBytes) != 32 {
		writeError(w, http.StatusBadRequest, "invalid public_key: must be 64 hex chars")
		return
	}

	// Verify device_id matches public key (defense in depth).
	expectedID := auth.DeriveDeviceID(pubBytes)
	if body.DeviceID != expectedID {
		writeError(w, http.StatusBadRequest, "device_id does not match public_key")
		return
	}

	// Claim the one-time provision token.
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

	// Check for existing camera by serial (re-provisioning to same or different server).
	existing, err := a.DB.GetCameraBySerial(r.Context(), body.DeviceSerial)
	if err != nil {
		slog.Error("provision: check existing failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if existing != nil {
		if existing.DeviceID != body.DeviceID {
			// Camera regenerated its keypair (unlikely). Update the device_id
			// by deleting the old camera and creating a new one.
			slog.Warn("provision: device_id changed for serial, recreating",
				"serial", body.DeviceSerial, "old_id", existing.DeviceID, "new_id", body.DeviceID)
			_ = a.DB.DeleteCamera(r.Context(), existing.DeviceID)
			if err := a.DB.CreateProvisionedCamera(r.Context(), body.DeviceID, *userID, body.DeviceSerial); err != nil {
				slog.Error("provision: create camera failed", "error", err)
				http.Error(w, "", http.StatusInternalServerError)
				return
			}
		} else {
			// Same device, possibly different server or user. Reassign ownership.
			if err := a.DB.ReassignCamera(r.Context(), body.DeviceID, *userID); err != nil {
				slog.Error("provision: reassign camera failed", "error", err)
				http.Error(w, "", http.StatusInternalServerError)
				return
			}
		}
	} else {
		if err := a.DB.CreateProvisionedCamera(r.Context(), body.DeviceID, *userID, body.DeviceSerial); err != nil {
			slog.Error("provision: create camera failed", "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
	}

	// Store the public key (upsert in case of re-provisioning).
	if err := a.DB.UpsertCameraPublicKey(r.Context(), body.DeviceID, body.PublicKey); err != nil {
		slog.Error("provision: store public key failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "camera_provisioned",
		"device_id", body.DeviceID, "user_id", *userID, "device_serial", body.DeviceSerial)

	writeJSON(w, http.StatusOK, common.ProvisionResponse{
		DeviceID: body.DeviceID,
		Status:   "registered",
	})
}
