package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/cargocam/ghostcam/server/apitypes"
)

// clientLogMaxBodyBytes caps the size of a single client-log request. Log
// entries should be small diagnostic strings, not full stack traces or
// serialised state — anything larger is rejected rather than silently
// truncated so the caller learns their payload was malformed.
const clientLogMaxBodyBytes = 8 * 1024

// ClientLog handles POST /api/v1/client-log. Accepts a single diagnostic
// entry from an authenticated viewer and writes it to the server's slog
// output. Intended for debugging mobile bugs where the user can't easily
// surface a desktop devtools console — the UI only posts when the
// "Client error logging" dev setting is enabled.
//
// Entries are rate-limited by the usual viewerAuth IP bucket in addition to
// the route-specific limiter; together they prevent a compromised or buggy
// client from flooding the log stream.
func (a *App) ClientLog(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, clientLogMaxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_failed")
		return
	}
	if len(body) > clientLogMaxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	}

	var entry apitypes.ClientLogEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}

	level := slog.LevelInfo
	switch entry.Level {
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	case "debug":
		level = slog.LevelDebug
	}

	attrs := []any{
		"component", "client-log",
		"user_id", getUserID(r),
		"source", entry.Source,
		"message", entry.Message,
	}
	if entry.UserAgent != "" {
		attrs = append(attrs, "user_agent", entry.UserAgent)
	}
	if entry.URL != "" {
		attrs = append(attrs, "url", entry.URL)
	}
	for k, v := range entry.Context {
		attrs = append(attrs, "ctx_"+k, v)
	}

	slog.Log(r.Context(), level, "client-log", attrs...)

	w.WriteHeader(http.StatusNoContent)
}
