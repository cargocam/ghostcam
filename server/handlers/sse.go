package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/redis"
	goredis "github.com/redis/go-redis/v9"
)

type telemetryEvent struct {
	DeviceID  string               `json:"device_id"`
	Telemetry *redis.TelemetryEntry `json:"telemetry"`
}

// SSE handles GET /events — Server-Sent Events stream for realtime telemetry.
func (h *Handlers) SSE(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	cameras, err := h.DB.ListCameras(r.Context(), userID)
	if err != nil {
		slog.Warn("SSE: failed to list cameras", "user_id", userID, "error", err)
		return
	}

	if len(cameras) == 0 || h.Redis == nil {
		// Keep connection alive with keepalive comments
		h.sseKeepAlive(r.Context(), w, flusher)
		return
	}

	rdb := h.Redis.RDB()

	// Build stream keys
	streamKeys := make([]string, 0, len(cameras))
	keyToDevice := make(map[string]string, len(cameras))
	for _, c := range cameras {
		key := fmt.Sprintf("telemetry:%s", c.DeviceID)
		streamKeys = append(streamKeys, key)
		keyToDevice[key] = c.DeviceID
	}

	// Start from latest
	lastIDs := make(map[string]string, len(streamKeys))
	for _, k := range streamKeys {
		lastIDs[k] = "$"
	}

	ctx := r.Context()
	keepAliveTicker := time.NewTicker(15 * time.Second)
	defer keepAliveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepAliveTicker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		default:
		}

		// Build XREAD args
		args := &goredis.XReadArgs{
			Streams: make([]string, 0, len(streamKeys)*2),
			Block:   5 * time.Second,
		}
		for _, k := range streamKeys {
			args.Streams = append(args.Streams, k)
		}
		for _, k := range streamKeys {
			args.Streams = append(args.Streams, lastIDs[k])
		}

		streams, err := rdb.XRead(ctx, args).Result()
		if err == goredis.Nil || err == context.DeadlineExceeded {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("SSE XREAD error", "user_id", userID, "error", err)
			time.Sleep(1 * time.Second)
			continue
		}

		for _, stream := range streams {
			deviceID, ok := keyToDevice[stream.Stream]
			if !ok {
				continue
			}

			for _, msg := range stream.Messages {
				lastIDs[stream.Stream] = msg.ID

				entry, err := redis.FieldsToEntry(msg.Values)
				if err != nil {
					continue
				}

				payload := telemetryEvent{
					DeviceID:  deviceID,
					Telemetry: entry,
				}

				jsonBytes, err := json.Marshal(payload)
				if err != nil {
					continue
				}

				fmt.Fprintf(w, "event: telemetry\ndata: %s\n\n", jsonBytes)
				flusher.Flush()
			}
		}
	}
}

func (h *Handlers) sseKeepAlive(ctx context.Context, w http.ResponseWriter, flusher http.Flusher) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
