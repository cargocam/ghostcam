package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/cargocam/ghostcam/api"
	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/redis"
)

// PostTelemetry handles POST /api/v1/cameras/{deviceID}/telemetry.
func (h *Handlers) PostTelemetry(w http.ResponseWriter, r *http.Request) {
	deviceID := ctxutil.GetCameraDeviceID(r)

	var body api.TelemetryPollRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Write telemetry to Redis
	if h.Redis != nil {
		redis.WriteTelemetry(r.Context(), h.Redis.RDB(), deviceID, &body.Telemetry)
	}

	// Claim pending commands
	commands, err := h.DB.ClaimCommands(r.Context(), deviceID)
	if err != nil {
		// Non-fatal: return empty commands
		commands = nil
	}

	var apiCommands []api.CameraCommand
	for _, raw := range commands {
		var cmd api.CameraCommand
		if json.Unmarshal(raw, &cmd) == nil {
			apiCommands = append(apiCommands, cmd)
		}
	}

	writeJSON(w, http.StatusOK, api.TelemetryPollResponse{Commands: apiCommands})
}
