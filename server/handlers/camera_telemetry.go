package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/cargocam/ghostcam/common"
	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/redis"
)

// PostTelemetry handles POST /api/v1/cameras/{deviceID}/telemetry.
func (h *Handlers) PostTelemetry(w http.ResponseWriter, r *http.Request) {
	deviceID := ctxutil.GetCameraDeviceID(r)

	var body common.TelemetryPollRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Write telemetry to Redis
	if h.Redis != nil {
		redis.WriteTelemetry(r.Context(), h.Redis.RDB(), deviceID, &body.Telemetry)
	}

	// Mark camera as seen (non-fatal)
	if err := h.DB.TouchCameraLastSeen(r.Context(), deviceID); err != nil {
		slog.Warn("failed to touch camera last_seen_at", "device_id", deviceID, "error", err)
	}

	// Claim pending commands
	commands, err := h.DB.ClaimCommands(r.Context(), deviceID)
	if err != nil {
		// Non-fatal: return empty commands
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
