package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/cargocam/ghostcam/common"
	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/go-chi/chi/v5"
	goredis "github.com/redis/go-redis/v9"
)

// wakeLiveKey is the Redis key the standby-mode wake-up signal lives at.
// Set with a 60 s TTL by WHEPOffer when a viewer hits a sleeping camera;
// read + deleted by PostTelemetry on the next poll. Short-lived so a
// stale signal can't keep waking a camera forever.
func wakeLiveKey(deviceID string) string {
	return "wake_live:" + deviceID
}

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

	// Mark camera as seen + store firmware version (non-fatal).
	if err := a.DB.TouchCameraLastSeen(r.Context(), deviceID, body.FwVersion); err != nil {
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

	// Nudge the camera to update if its firmware is stale. The camera
	// handles "update_firmware" by calling CheckFirmwareUpdate, which
	// downloads, SHA-verifies, stages, and exits for systemd restart.
	if body.FwVersion != "" && a.Redis != nil {
		latest, _ := a.Redis.Get(r.Context(), "firmware:latest:version").Result()
		if latest != "" && latest != body.FwVersion {
			apiCommands = append(apiCommands, common.CameraCommand{Type: "update_firmware"})
		}
	}

	// Standby-mode wake: read + delete the wake_live flag in one round-
	// trip. WHEPOffer sets this when a viewer arrives at a camera with
	// no live session. Camera honours it by opening the live WS.
	wakeLive := false
	if a.Redis != nil {
		_, err := a.Redis.GetDel(r.Context(), wakeLiveKey(deviceID)).Result()
		if err == nil {
			wakeLive = true
		} else if err != goredis.Nil {
			slog.Debug("wake_live get-del", "device_id", deviceID, "error", err)
		}
	}

	// Lazy-mode scrub fulfilment: any local-only segments overlapping a
	// recently-requested timeline range trigger an `upload_segments`
	// command piggy-backed on this telemetry response. The scrub
	// handler parks the (deviceID, from, to) tuple in Redis under
	// `pending_upload:<deviceID>` for one telemetry-poll interval; we
	// pop and convert to a command here so the camera fetches on the
	// next cycle.
	if a.Redis != nil {
		key := pendingUploadKey(deviceID)
		ids, err := a.Redis.SMembers(r.Context(), key).Result()
		if err == nil && len(ids) > 0 {
			a.Redis.Del(r.Context(), key)
			apiCommands = append(apiCommands, common.CameraCommand{
				Type:       "upload_segments",
				SegmentIDs: ids,
			})
		}
	}

	writeJSON(w, http.StatusOK, common.TelemetryPollResponse{
		Commands: apiCommands,
		WakeLive: wakeLive,
	})
}

// pendingUploadKey holds the set of segment IDs the viewer has scrubbed
// to but the camera hasn't yet uploaded. Lives in Redis as a Set so
// concurrent scrub events naturally de-dupe. Cleared on the next
// telemetry poll (when we convert to an upload_segments command).
func pendingUploadKey(deviceID string) string {
	return "pending_upload:" + deviceID
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

	writeJSON(w, http.StatusOK, apitypes.TelemetryRangeResponse{Entries: entries})
}
