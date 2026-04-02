package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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

	// Disable write deadline for SSE — this is a long-lived connection
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

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

	// Build device ID set for filtering motion events
	deviceIDs := make(map[string]bool, len(cameras))
	for _, c := range cameras {
		deviceIDs[c.DeviceID] = true
	}

	// Subscribe to motion_detected pub/sub
	motionCh := make(chan string, 32)
	pubsub := rdb.Subscribe(ctx, "motion_detected", "storage_capped")
	go func() {
		defer pubsub.Close()
		ch := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				select {
				case motionCh <- msg.Channel + "|" + msg.Payload:
				default:
				}
			}
		}
	}()

	keepAliveTicker := time.NewTicker(15 * time.Second)
	defer keepAliveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepAliveTicker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case raw := <-motionCh:
			parts := splitOnce(raw, "|")
			channel, payload := parts[0], parts[1]
			switch channel {
			case "motion_detected":
				// Filter to only this user's cameras
				var motionData struct {
					DeviceID string `json:"device_id"`
				}
				if json.Unmarshal([]byte(payload), &motionData) == nil && deviceIDs[motionData.DeviceID] {
					fmt.Fprintf(w, "event: motion_detected\ndata: %s\n\n", payload)
					flusher.Flush()
				}
			case "storage_capped":
				// Storage capped events carry user_id — match current user
				var capData struct {
					UserID string `json:"user_id"`
				}
				if json.Unmarshal([]byte(payload), &capData) == nil && capData.UserID == userID {
					fmt.Fprintf(w, "event: storage_capped\ndata: %s\n\n", payload)
					flusher.Flush()
				}
			}
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

func splitOnce(s, sep string) [2]string {
	i := strings.Index(s, sep)
	if i < 0 {
		return [2]string{s, ""}
	}
	return [2]string{s[:i], s[i+len(sep):]}
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
