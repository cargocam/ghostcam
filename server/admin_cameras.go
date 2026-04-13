package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/go-chi/chi/v5"
)

// AdminListCameras handles GET /api/v1/admin/cameras.
//
// Returns every camera in the database joined with its owner's email.
// Includes cameras belonging to soft-deleted users so admins can act
// on orphans.
func (a *App) AdminListCameras(w http.ResponseWriter, r *http.Request) {
	cams, err := a.DB.ListAllCameras(r.Context())
	if err != nil {
		slog.Error("admin: list cameras failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	out := make([]apitypes.AdminCamera, 0, len(cams))
	for _, c := range cams {
		out = append(out, apitypes.AdminCamera{
			DeviceID:    c.DeviceID,
			DisplayName: c.DisplayName,
			UserID:      c.UserID,
			OwnerEmail:  c.OwnerEmail,
			EnrolledAt:  c.EnrolledAt,
			LastSeenAt:  c.LastSeenAt,
		})
	}
	writeJSON(w, http.StatusOK, apitypes.AdminListCamerasResponse{Cameras: out})
}

// AdminReassignCamera handles PATCH /api/v1/admin/cameras/{deviceID}.
//
// Moves a camera to a different owning user. Rejects with 409 if the
// target user is already at or over their camera limit — the alternative
// would be "reassignment seems to work but the camera silently stops
// uploading on its next presign." Admins who really want to move a
// camera into an over-limit user must archive one of that user's
// existing cameras first.
//
// Tier resolution runs through the normal `effectiveTier` path so it
// honors the same fail-closed rules as everywhere else: unknown Stripe
// price IDs resolve to free.
func (a *App) AdminReassignCamera(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "missing_device_id")
		return
	}

	var body apitypes.AdminReassignCameraRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.UserID == "" {
		writeError(w, http.StatusBadRequest, "missing_user_id")
		return
	}

	// Target user must exist and not be soft-deleted — otherwise the
	// move immediately strands the camera.
	target, err := a.DB.GetUserByID(r.Context(), body.UserID)
	if err != nil {
		slog.Error("admin: get target user failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if target == nil || target.DeletedAt != nil {
		writeError(w, http.StatusNotFound, "target_user_not_found")
		return
	}

	// Look up the camera so we can ignore no-op moves and short-circuit
	// the tier check if the caller is passing the current owner.
	cam, err := a.DB.GetCamera(r.Context(), deviceID)
	if err != nil {
		slog.Error("admin: get camera failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if cam == nil {
		writeError(w, http.StatusNotFound, "camera_not_found")
		return
	}
	if cam.UserID != nil && *cam.UserID == body.UserID {
		// No-op: already owned by the requested target.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// Tier limit check: resolve the target user's effective tier and
	// compare their current camera count against it. A nil CameraLimit
	// means unlimited — always allowed.
	targetSub, err := a.DB.GetSubscription(r.Context(), body.UserID)
	if err != nil {
		slog.Error("admin: get target subscription failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	targetTier := a.effectiveTier(r.Context(), targetSub)
	if targetTier.CameraLimit != nil {
		count, err := a.DB.GetCameraCount(r.Context(), body.UserID)
		if err != nil {
			slog.Error("admin: get target camera count failed", "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		if count >= int64(*targetTier.CameraLimit) {
			writeJSON(w, http.StatusConflict, apitypes.AdminReassignCameraConflictResponse{
				Error:       "tier_limit_exceeded",
				CameraLimit: *targetTier.CameraLimit,
				CameraCount: count,
			})
			return
		}
	}

	if err := a.DB.ReassignCamera(r.Context(), deviceID, body.UserID); err != nil {
		slog.Error("admin: reassign camera failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "admin_camera_reassign",
		"actor", getUserEmail(r), "device_id", deviceID,
		"from_user_id", derefUserID(cam), "to_user_id", body.UserID)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// AdminDeleteCamera handles DELETE /api/v1/admin/cameras/{deviceID}.
//
// Unscoped delete — unlike the per-user DELETE /api/v1/cameras/{deviceID},
// this endpoint does not care about ownership. Reaps the camera's S3
// segment objects synchronously before removing the `cameras` row so
// admin-initiated deletion doesn't leave orphaned storage behind; the
// DB cascade then drops segments, camera_api_keys, enrollment_tokens.
func (a *App) AdminDeleteCamera(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "missing_device_id")
		return
	}

	cam, err := a.DB.GetCamera(r.Context(), deviceID)
	if err != nil {
		slog.Error("admin: get camera failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if cam == nil {
		writeError(w, http.StatusNotFound, "camera_not_found")
		return
	}

	a.purgeAllFootageForDelete(r.Context(), deviceID, derefUserID(cam))

	if err := a.DB.DeleteCamera(r.Context(), deviceID); err != nil {
		slog.Error("admin: delete camera failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "admin_camera_delete",
		"actor", getUserEmail(r), "device_id", deviceID,
		"owner_user_id", derefUserID(cam))

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func derefUserID(c *db.CameraRecord) string {
	if c == nil || c.UserID == nil {
		return ""
	}
	return *c.UserID
}
