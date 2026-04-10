package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/cargocam/ghostcam/common"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/go-chi/chi/v5"
)

// PostTelemetry handles POST /api/v1/cameras/{deviceID}/telemetry.
func (a *App) PostTelemetry(w http.ResponseWriter, r *http.Request) {
	deviceID := getCameraDeviceID(r)

	var body common.TelemetryPollRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Write telemetry to Redis.
	if a.Redis != nil {
		redis.WriteTelemetry(r.Context(), a.Redis, deviceID, &body.Telemetry)
	}

	// Mark camera as seen (non-fatal).
	if err := a.DB.TouchCameraLastSeen(r.Context(), deviceID); err != nil {
		slog.Warn("failed to touch camera last_seen_at", "device_id", deviceID, "error", err)
	}

	// Claim pending commands (atomically deletes them).
	commands, err := a.DB.ClaimCommands(r.Context(), deviceID)
	if err != nil {
		commands = nil
	}

	var apiCommands []common.CameraCommand
	for _, raw := range commands {
		var cmd common.CameraCommand
		if json.Unmarshal(raw, &cmd) == nil {
			apiCommands = append(apiCommands, cmd)
		}
	}

	writeJSON(w, http.StatusOK, common.TelemetryPollResponse{Commands: apiCommands})
}

// GetTelemetryLatest handles GET /api/v1/telemetry/{deviceID}/latest
func (a *App) GetTelemetryLatest(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if _, ok := a.ownedCamera(w, r, deviceID); !ok {
		return
	}

	if a.Redis == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	entry, err := redis.QueryTelemetryLatest(r.Context(), a.Redis, deviceID)
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
func (a *App) GetTelemetryRange(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if _, ok := a.ownedCamera(w, r, deviceID); !ok {
		return
	}

	if a.Redis == nil {
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}

	fromMs, _ := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
	toMs, _ := strconv.ParseUint(r.URL.Query().Get("to"), 10, 64)
	limit, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if limit <= 0 {
		limit = 600
	}

	entries, err := redis.QueryTelemetryRange(r.Context(), a.Redis, deviceID, fromMs, toMs, limit)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, struct {
		Entries []redis.TelemetryEntry `json:"entries"`
	}{Entries: entries})
}
