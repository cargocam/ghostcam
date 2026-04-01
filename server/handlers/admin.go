package handlers

import (
	"net/http"
)

// ReloadConfig handles POST /api/v1/admin/reload.
func (h *Handlers) ReloadConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "reload not implemented"})
}

// FirmwareLatest handles GET /api/v1/firmware/latest.
func (h *Handlers) FirmwareLatest(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"release": nil})
}

// GithubWebhook handles POST /api/v1/webhooks/github.
func (h *Handlers) GithubWebhook(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// QueryAudit handles GET /api/v1/audit.
func (h *Handlers) QueryAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	eventType := r.URL.Query().Get("type")
	since := r.URL.Query().Get("since")
	until := r.URL.Query().Get("until")
	limit := parseQueryUint64(r, "limit", 50)
	offset := parseQueryUint64(r, "offset", 0)

	entries, total, err := h.DB.QueryAuditLog(ctx, eventType, since, until, int64(limit), int64(offset))
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   total,
	})
}
