package handlers

import (
	"net/http"
	"strconv"

	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/go-chi/chi/v5"
)

// GetTelemetryLatest handles GET /api/v1/telemetry/{deviceID}/latest
func (h *Handlers) GetTelemetryLatest(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	deviceID := chi.URLParam(r, "deviceID")

	// Verify ownership
	camera, err := h.DB.GetCamera(r.Context(), deviceID)
	if err != nil || camera == nil || camera.UserID == nil || *camera.UserID != userID {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	if h.Redis == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	entry, err := redis.QueryTelemetryLatest(r.Context(), h.Redis.RDB(), deviceID)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if entry == nil {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, entry)
}

// GetTelemetryRange handles GET /api/v1/telemetry/{deviceID}?from=&to=&limit=
func (h *Handlers) GetTelemetryRange(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)
	deviceID := chi.URLParam(r, "deviceID")

	camera, err := h.DB.GetCamera(r.Context(), deviceID)
	if err != nil || camera == nil || camera.UserID == nil || *camera.UserID != userID {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	if h.Redis == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	fromMs, _ := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
	toMs, _ := strconv.ParseUint(r.URL.Query().Get("to"), 10, 64)
	limit, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if limit <= 0 {
		limit = 600
	}

	entries, err := redis.QueryTelemetryRange(r.Context(), h.Redis.RDB(), deviceID, fromMs, toMs, limit)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, struct {
		Entries []redis.TelemetryEntry `json:"entries"`
	}{Entries: entries})
}
