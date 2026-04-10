package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/redis"
	goredis "github.com/redis/go-redis/v9"
)

// SSE handles GET /events — Server-Sent Events stream for realtime telemetry.
//
// On connect, emits the latest telemetry for each camera so clients have
// immediate state without waiting for the next camera poll cycle. The ts and
// server_ts fields in each telemetry entry let the client determine freshness
// and infer online/offline status.
func (a *App) SSE(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	cameras, err := a.DB.ListCameras(r.Context(), userID)
	if err != nil {
		slog.Warn("SSE: failed to list cameras", "user_id", userID, "error", err)
		return
	}

	if len(cameras) == 0 || a.Redis == nil {
		sseKeepAlive(r.Context(), w, flusher)
		return
	}

	rdb := a.Redis
	ctx := r.Context()

	streamKeys := make([]string, 0, len(cameras))
	keyToDevice := make(map[string]string, len(cameras))
	for _, c := range cameras {
		key := fmt.Sprintf("telemetry:%s", c.DeviceID)
		streamKeys = append(streamKeys, key)
		keyToDevice[key] = c.DeviceID
	}

	// Emit initial telemetry burst for each camera.
	lastIDs := make(map[string]string, len(streamKeys))
	for _, k := range streamKeys {
		deviceID := keyToDevice[k]
		entry, err := redis.QueryTelemetryLatest(ctx, rdb, deviceID)
		if err == nil && entry != nil {
			payload := apitypes.TelemetryStreamEvent{DeviceID: deviceID, Telemetry: entry}
			jsonBytes, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: telemetry\ndata: %s\n\n", jsonBytes)
		}
		lastIDs[k] = "$"
	}
	flusher.Flush()

	// Subscribe to per-user pub/sub channels.
	eventCh := make(chan string, 32)
	pubsub := rdb.Subscribe(ctx,
		fmt.Sprintf("motion:%s", userID),
		fmt.Sprintf("storage_capped:%s", userID),
		fmt.Sprintf("coverage:%s", userID),
		fmt.Sprintf("events_sync:%s", userID),
	)
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
				case eventCh <- msg.Channel + "|" + msg.Payload:
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
		case raw := <-eventCh:
			parts := splitOnce(raw, "|")
			channel, payload := parts[0], parts[1]
			var eventType string
			if len(channel) > 0 {
				switch channel[0] {
				case 'm':
					eventType = "motion_detected"
				case 's':
					eventType = "storage_capped"
				case 'c':
					eventType = "coverage"
				case 'e':
					eventType = "events_sync"
				}
			}
			if eventType != "" {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload)
				flusher.Flush()
			}
		default:
		}

		// XREAD telemetry streams.
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

				payload := apitypes.TelemetryStreamEvent{
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
	for i := 0; i < len(s); i++ {
		if s[i] == sep[0] {
			return [2]string{s[:i], s[i+1:]}
		}
	}
	return [2]string{s, ""}
}

func sseKeepAlive(ctx context.Context, w http.ResponseWriter, flusher http.Flusher) {
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
