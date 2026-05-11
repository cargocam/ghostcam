package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/common"
	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/auth"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/go-chi/chi/v5"
)

const provisionTokenTTLSecs = 24 * 3600 // 24 hours

// ListCameras handles GET /api/v1/cameras.
func (a *App) ListCameras(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	cameras, err := a.DB.ListCameras(r.Context(), userID)
	if err != nil {
		slog.Error("list cameras failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	resp := make([]apitypes.CameraResponse, 0, len(cameras))
	for _, c := range cameras {
		cr := apitypes.CameraResponse{
			DeviceID:      c.DeviceID,
			DisplayName:   c.DisplayName,
			EnrolledAt:    uint64(c.EnrolledAt),
			LastSeenAt:    c.LastSeenAt,
			Provisioned:   c.LastSeenAt != nil,
			Notes:         c.Notes,
			Resolution:    c.Resolution,
			RecordingMode: c.RecordingMode,
			FwVersion:     derefStr(c.FwVersion),
			PowerMode:     c.PowerMode,
			UploadMode:    c.UploadMode,
			Schedule:      c.Schedule,
			BatteryRules:  c.BatteryRules,
		}
		if a.Redis != nil {
			entry, _ := redis.QueryTelemetryLatest(ctx, a.Redis, c.DeviceID)
			cr.Telemetry = entry
		}
		resp = append(resp, cr)
	}
	writeJSON(w, http.StatusOK, resp)
}

// Enroll handles POST /api/v1/cameras (generate provision token).
func (a *App) Enroll(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	ctx := r.Context()

	// Enforce camera limit based on tier.
	sub, _ := a.DB.GetSubscription(ctx, userID)
	tier := a.effectiveTier(ctx, sub)
	if tier.CameraLimit != nil {
		count, err := a.DB.GetCameraCount(ctx, userID)
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

	// Consume request body (ignored but accepted for backward compat).
	var body json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)

	rawToken := auth.GenerateRandomPassword()
	tokenHash := auth.HMACToken(rawToken, a.HMACSecret)

	now := uint64(time.Now().Unix())
	expiresAt := now + provisionTokenTTLSecs

	if err := a.DB.CreateProvisionToken(r.Context(), tokenHash, userID, int64(expiresAt)); err != nil {
		slog.Error("enroll: create provision token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "enrollment_started", "user_id", userID)
	writeJSON(w, http.StatusOK, apitypes.EnrollResponse{Token: rawToken, ExpiresAt: expiresAt})
}

// ownedCamera looks up `deviceID` and verifies the authenticated viewer owns
// it. On any failure — DB error, missing row, wrong owner, or missing auth
// context — it writes a 404 and returns (nil, false). Callers use
// `if !ok { return }`. Presign uses a different check (camera auth, not
// viewer auth) and does not go through here.
func (a *App) ownedCamera(w http.ResponseWriter, r *http.Request, deviceID string) (*db.CameraRecord, bool) {
	userID := getUserID(r)
	// Defensive: routes are gated by viewerAuth, but refuse to accept an
	// empty userID so a misconfigured route can never let an unauthenticated
	// request match a camera whose user_id happens to be "".
	if userID == "" {
		http.Error(w, "", http.StatusUnauthorized)
		return nil, false
	}
	camera, err := a.DB.GetCamera(r.Context(), deviceID)
	if err != nil || camera == nil || camera.UserID == nil || *camera.UserID != userID {
		http.Error(w, "", http.StatusNotFound)
		return nil, false
	}
	return camera, true
}

// GetCamera handles GET /api/v1/cameras/{deviceID}.
func (a *App) GetCamera(w http.ResponseWriter, r *http.Request) {
	camera, ok := a.ownedCamera(w, r, chi.URLParam(r, "deviceID"))
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, apitypes.CameraResponse{
		DeviceID:      camera.DeviceID,
		DisplayName:   camera.DisplayName,
		EnrolledAt:    uint64(camera.EnrolledAt),
		LastSeenAt:    camera.LastSeenAt,
		Provisioned:   camera.LastSeenAt != nil,
		Notes:         camera.Notes,
		Resolution:    camera.Resolution,
		RecordingMode: camera.RecordingMode,
		FwVersion:     derefStr(camera.FwVersion),
		PowerMode:     camera.PowerMode,
		UploadMode:    camera.UploadMode,
		Schedule:      camera.Schedule,
		BatteryRules:  camera.BatteryRules,
	})
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// UpdateCamera handles PATCH /api/v1/cameras/{deviceID}.
func (a *App) UpdateCamera(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	camera, ok := a.ownedCamera(w, r, deviceID)
	if !ok {
		return
	}

	var body apitypes.UpdateCameraRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Resolution != nil {
		switch *body.Resolution {
		case "480p", "720p", "1080p":
		default:
			writeError(w, http.StatusBadRequest, "resolution must be 480p, 720p, or 1080p")
			return
		}
	}

	if body.RecordingMode != nil {
		switch *body.RecordingMode {
		case "constant", "motion", "never":
		default:
			writeError(w, http.StatusBadRequest, "recording_mode must be constant, motion, or never")
			return
		}
	}

	if body.PowerMode != nil {
		switch *body.PowerMode {
		case "live", "standby", "sleep":
		default:
			writeError(w, http.StatusBadRequest, "power_mode must be live, standby, or sleep")
			return
		}
	}
	if body.UploadMode != nil {
		switch *body.UploadMode {
		case "proactive", "lazy":
		default:
			writeError(w, http.StatusBadRequest, "upload_mode must be proactive or lazy")
			return
		}
	}

	ctx := r.Context()

	// schedule / battery_rules: convert json.RawMessage to *[]byte so the
	// CameraUpdate carrier can distinguish "leave alone" (nil) from
	// "clear" (empty bytes).
	var schedPtr, batteryPtr *[]byte
	if body.Schedule != nil {
		sb := []byte(body.Schedule)
		schedPtr = &sb
	}
	if body.BatteryRules != nil {
		bb := []byte(body.BatteryRules)
		batteryPtr = &bb
	}

	if err := a.DB.UpdateCamera(ctx, deviceID, &db.CameraUpdate{
		DisplayName:   body.DisplayName,
		Notes:         body.Notes,
		Resolution:    body.Resolution,
		RecordingMode: body.RecordingMode,
		PowerMode:     body.PowerMode,
		UploadMode:    body.UploadMode,
		Schedule:      schedPtr,
		BatteryRules:  batteryPtr,
	}); err != nil {
		slog.Error("update camera failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Enqueue commands for settings that require camera-side state
	// changes. Power-mode commands DON'T restart the daemon — the
	// camera applies them in-process — so they're cheap to issue on
	// every change.
	if body.Resolution != nil && *body.Resolution != camera.Resolution {
		cmd, _ := json.Marshal(common.CameraCommand{Type: "set_resolution", Resolution: *body.Resolution})
		if err := a.DB.EnqueueCommand(ctx, deviceID, cmd); err != nil {
			slog.Error("enqueue set_resolution failed", "error", err)
		}
	}
	if body.RecordingMode != nil && *body.RecordingMode != camera.RecordingMode {
		cmd, _ := json.Marshal(common.CameraCommand{Type: "set_recording_mode", Mode: *body.RecordingMode})
		if err := a.DB.EnqueueCommand(ctx, deviceID, cmd); err != nil {
			slog.Error("enqueue set_recording_mode failed", "error", err)
		}
	}
	if body.PowerMode != nil && *body.PowerMode != camera.PowerMode {
		cmd, _ := json.Marshal(common.CameraCommand{Type: "set_power_mode", PowerMode: *body.PowerMode})
		if err := a.DB.EnqueueCommand(ctx, deviceID, cmd); err != nil {
			slog.Error("enqueue set_power_mode failed", "error", err)
		}
	}
	if body.UploadMode != nil && *body.UploadMode != camera.UploadMode {
		cmd, _ := json.Marshal(common.CameraCommand{Type: "set_upload_mode", UploadMode: *body.UploadMode})
		if err := a.DB.EnqueueCommand(ctx, deviceID, cmd); err != nil {
			slog.Error("enqueue set_upload_mode failed", "error", err)
		}
	}
	if body.Schedule != nil {
		cmd, _ := json.Marshal(common.CameraCommand{Type: "set_schedule", Schedule: string(body.Schedule)})
		if err := a.DB.EnqueueCommand(ctx, deviceID, cmd); err != nil {
			slog.Error("enqueue set_schedule failed", "error", err)
		}
	}
	if body.BatteryRules != nil {
		cmd, _ := json.Marshal(common.CameraCommand{Type: "set_battery_rules", BatteryRules: string(body.BatteryRules)})
		if err := a.DB.EnqueueCommand(ctx, deviceID, cmd); err != nil {
			slog.Error("enqueue set_battery_rules failed", "error", err)
		}
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteCamera handles DELETE /api/v1/cameras/{deviceID}.
//
// Reaps the camera's S3 segment objects synchronously before removing
// the `cameras` row. The DB cascade on segments would have deleted the
// rows either way, but without the explicit S3 purge the underlying
// objects were silently orphaned — the presign-path opportunistic
// prune requires the camera to still be uploading, which it will never
// do again after this call.
func (a *App) DeleteCamera(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	camera, ok := a.ownedCamera(w, r, deviceID)
	if !ok {
		return
	}

	userID := ""
	if camera.UserID != nil {
		userID = *camera.UserID
	}
	a.purgeAllFootageForDelete(r.Context(), deviceID, userID)

	if err := a.DB.DeleteCamera(r.Context(), deviceID); err != nil {
		slog.Error("delete camera failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "camera_unregistered", "device_id", deviceID, "initiated_by", getUserID(r))
	w.WriteHeader(http.StatusOK)
}
