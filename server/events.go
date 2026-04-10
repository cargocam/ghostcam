package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/cargocam/ghostcam/server/redis"
	"github.com/go-chi/chi/v5"
)

// ListEvents handles GET /api/v1/events?count=&before=
func (a *App) ListEvents(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	if a.Redis == nil {
		writeJSON(w, http.StatusOK, map[string]any{"events": []any{}})
		return
	}

	count, _ := strconv.ParseInt(r.URL.Query().Get("count"), 10, 64)
	if count <= 0 || count > 200 {
		count = 50
	}
	beforeID := r.URL.Query().Get("before")

	events, err := redis.ListEvents(r.Context(), a.Redis.RDB(), userID, count, beforeID)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// GetUnreadCount handles GET /api/v1/events/unread
func (a *App) GetUnreadCount(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	if a.Redis == nil {
		writeJSON(w, http.StatusOK, map[string]any{"count": 0})
		return
	}

	count, _ := redis.UnreadCount(r.Context(), a.Redis.RDB(), userID)
	writeJSON(w, http.StatusOK, map[string]any{"count": count})
}

// MarkEventRead handles PATCH /api/v1/events/{eventID}/read
func (a *App) MarkEventRead(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	eventID := chi.URLParam(r, "eventID")
	if a.Redis == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()
	_, err := redis.MarkEventRead(ctx, a.Redis.RDB(), userID, eventID)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	syncPayload, _ := json.Marshal(map[string]string{"action": "read", "event_id": eventID})
	a.Redis.RDB().Publish(ctx, fmt.Sprintf("events_sync:%s", userID), syncPayload)

	w.WriteHeader(http.StatusOK)
}

// MarkAllEventsRead handles POST /api/v1/events/read-all
func (a *App) MarkAllEventsRead(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	if a.Redis == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()
	redis.MarkAllRead(ctx, a.Redis.RDB(), userID)

	syncPayload, _ := json.Marshal(map[string]string{"action": "read_all"})
	a.Redis.RDB().Publish(ctx, fmt.Sprintf("events_sync:%s", userID), syncPayload)

	w.WriteHeader(http.StatusOK)
}

// DismissEvent handles DELETE /api/v1/events/{eventID}
func (a *App) DismissEvent(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	eventID := chi.URLParam(r, "eventID")
	if a.Redis == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()
	redis.DismissEvent(ctx, a.Redis.RDB(), userID, eventID)

	syncPayload, _ := json.Marshal(map[string]string{"action": "dismiss", "event_id": eventID})
	a.Redis.RDB().Publish(ctx, fmt.Sprintf("events_sync:%s", userID), syncPayload)

	w.WriteHeader(http.StatusOK)
}
