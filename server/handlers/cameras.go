package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/server/auth"
	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/go-chi/chi/v5"
)

const provisionTokenTTLSecs = 24 * 3600 // 24 hours

type cameraResponse struct {
	DeviceID      string  `json:"device_id"`
	DisplayName   string  `json:"display_name"`
	EnrolledAt    uint64  `json:"enrolled_at"`
	LastSeenAt    *int64  `json:"last_seen_at,omitempty"`
	Provisioned   bool    `json:"provisioned"`
	Notes         *string `json:"notes,omitempty"`
	Resolution    string  `json:"resolution"`
	RecordingMode string  `json:"recording_mode"`
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
			DeviceID:      c.DeviceID,
			DisplayName:   c.DisplayName,
			EnrolledAt:    uint64(c.EnrolledAt),
			LastSeenAt:    c.LastSeenAt,
			Provisioned:   c.LastSeenAt != nil,
			Notes:         c.Notes,
			Resolution:    c.Resolution,
			RecordingMode: c.RecordingMode,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// Enroll handles POST /api/v1/cameras (generate provision token).
func (h *Handlers) Enroll(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	ctx := r.Context()

	// Enforce camera limit based on tier
	sub, _ := h.DB.GetSubscription(ctx, userID)
	tier := billing.GetTier(effectiveTier(sub, h.Stripe.SecretKey != ""))
	if tier.CameraLimit != nil {
		count, err := h.DB.GetCameraCount(ctx, userID)
		if err != nil {
			slog.Error("enroll: get camera count failed", "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		if count >= int64(*tier.CameraLimit) {
			writeError(w, http.StatusPaymentRequired, "camera_limit_reached")
			return
		}
	}

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
		DeviceID:      camera.DeviceID,
		DisplayName:   camera.DisplayName,
		EnrolledAt:    uint64(camera.EnrolledAt),
		LastSeenAt:    camera.LastSeenAt,
		Provisioned:   camera.LastSeenAt != nil,
		Notes:         camera.Notes,
		Resolution:    camera.Resolution,
		RecordingMode: camera.RecordingMode,
	})
}

type updateCameraRequest struct {
	DisplayName   *string `json:"display_name,omitempty"`
	Notes         *string `json:"notes,omitempty"`
	Resolution    *string `json:"resolution,omitempty"`
	RecordingMode *string `json:"recording_mode,omitempty"`
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

	// Validate resolution
	if body.Resolution != nil {
		switch *body.Resolution {
		case "480p", "720p", "1080p":
		default:
			writeError(w, http.StatusBadRequest, "resolution must be 480p, 720p, or 1080p")
			return
		}
	}

	// Validate recording mode
	if body.RecordingMode != nil {
		switch *body.RecordingMode {
		case "constant", "motion":
		default:
			writeError(w, http.StatusBadRequest, "recording_mode must be constant or motion")
			return
		}
	}

	ctx := r.Context()

	if err := h.DB.UpdateCamera(ctx, deviceID, &db.CameraUpdate{
		DisplayName:   body.DisplayName,
		Notes:         body.Notes,
		Resolution:    body.Resolution,
		RecordingMode: body.RecordingMode,
	}); err != nil {
		slog.Error("update camera failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Enqueue commands for settings that require camera restart
	if body.Resolution != nil && *body.Resolution != camera.Resolution {
		cmd, _ := json.Marshal(map[string]string{"type": "set_resolution", "resolution": *body.Resolution})
		if err := h.DB.EnqueueCommand(ctx, deviceID, cmd); err != nil {
			slog.Error("enqueue set_resolution failed", "error", err)
		}
	}
	if body.RecordingMode != nil && *body.RecordingMode != camera.RecordingMode {
		cmd, _ := json.Marshal(map[string]string{"type": "set_recording_mode", "mode": *body.RecordingMode})
		if err := h.DB.EnqueueCommand(ctx, deviceID, cmd); err != nil {
			slog.Error("enqueue set_recording_mode failed", "error", err)
		}
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
