package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/server/auth"
	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/go-chi/chi/v5"
)

const provisionTokenTTLSecs = 24 * 3600 // 24 hours

type cameraResponse struct {
	DeviceID    string  `json:"device_id"`
	DisplayName string  `json:"display_name"`
	EnrolledAt  uint64  `json:"enrolled_at"`
	Notes       *string `json:"notes,omitempty"`
}

type enrollResponse struct {
	Token     string `json:"token"`
	ExpiresAt uint64 `json:"expires_at"`
}

// ListCameras handles GET /api/v1/cameras.
func (h *Handlers) ListCameras(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)

	cameras, err := h.DB.ListCameras(r.Context(), userID)
	if err != nil {
		slog.Error("list cameras failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	resp := make([]cameraResponse, 0, len(cameras))
	for _, c := range cameras {
		resp = append(resp, cameraResponse{
			DeviceID:    c.DeviceID,
			DisplayName: c.DisplayName,
			EnrolledAt:  uint64(c.EnrolledAt),
			Notes:       c.Notes,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// Enroll handles POST /api/v1/cameras (generate provision token).
func (h *Handlers) Enroll(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)

	// Consume request body (ignored but accepted for backward compat)
	var body json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)

	rawToken := auth.GenerateRandomPassword()
	tokenHash := auth.HMACToken(rawToken, h.HMACSecret)

	now := uint64(time.Now().Unix())
	expiresAt := now + provisionTokenTTLSecs

	if err := h.DB.CreateProvisionToken(r.Context(), tokenHash, userID, int64(expiresAt)); err != nil {
		slog.Error("enroll: create provision token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	db.AuditLog("enrollment_started", "user_id", userID)
	writeJSON(w, http.StatusOK, enrollResponse{Token: rawToken, ExpiresAt: expiresAt})
}

// GetCamera handles GET /api/v1/cameras/{deviceID}.
func (h *Handlers) GetCamera(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	deviceID := chi.URLParam(r, "deviceID")

	camera, err := h.DB.GetCamera(r.Context(), deviceID)
	if err != nil {
		slog.Error("get camera failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if camera == nil || camera.UserID == nil || *camera.UserID != userID {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, cameraResponse{
		DeviceID:    camera.DeviceID,
		DisplayName: camera.DisplayName,
		EnrolledAt:  uint64(camera.EnrolledAt),
		Notes:       camera.Notes,
	})
}

type updateCameraRequest struct {
	DisplayName *string `json:"display_name,omitempty"`
	Notes       *string `json:"notes,omitempty"`
}

// UpdateCamera handles PATCH /api/v1/cameras/{deviceID}.
func (h *Handlers) UpdateCamera(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	deviceID := chi.URLParam(r, "deviceID")

	// Verify ownership
	camera, err := h.DB.GetCamera(r.Context(), deviceID)
	if err != nil || camera == nil || camera.UserID == nil || *camera.UserID != userID {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	var body updateCameraRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.DB.UpdateCamera(r.Context(), deviceID, &db.CameraUpdate{
		DisplayName: body.DisplayName,
		Notes:       body.Notes,
	}); err != nil {
		slog.Error("update camera failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteCamera handles DELETE /api/v1/cameras/{deviceID}.
func (h *Handlers) DeleteCamera(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	deviceID := chi.URLParam(r, "deviceID")

	camera, err := h.DB.GetCamera(r.Context(), deviceID)
	if err != nil || camera == nil || camera.UserID == nil || *camera.UserID != userID {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	if err := h.DB.DeleteCamera(r.Context(), deviceID); err != nil {
		slog.Error("delete camera failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	db.AuditLog("camera_unregistered", "device_id", deviceID, "initiated_by", userID)
	w.WriteHeader(http.StatusOK)
}
