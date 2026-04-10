package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/cargocam/ghostcam/server/s3"
)

const maxFirmwareSize = 50 * 1024 * 1024 // 50MB

// ReloadConfig handles POST /api/v1/admin/reload.
func (a *App) ReloadConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "reload not implemented"})
}

// FirmwareLatest handles GET /api/v1/firmware/latest (public, no auth).
// Returns the latest firmware version and a presigned download URL from Tigris.
func (a *App) FirmwareLatest(w http.ResponseWriter, r *http.Request) {
	if a.Redis == nil {
		writeJSON(w, http.StatusOK, map[string]any{"release": nil})
		return
	}

	ctx := r.Context()
	version, err := a.Redis.Get(ctx, "firmware:latest:version").Result()
	if err != nil || version == "" {
		writeJSON(w, http.StatusOK, map[string]any{"release": nil})
		return
	}

	key := s3.FirmwareKey(version)
	downloadURL, err := a.S3.PresignGet(ctx, key)
	if err != nil {
		slog.Warn("firmware: presign GET failed", "version", version, "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"release": nil})
		return
	}

	sha256hex, _ := a.Redis.Get(ctx, "firmware:latest:sha256").Result()

	release := map[string]any{
		"version":      version,
		"download_url": downloadURL,
	}
	if sha256hex != "" {
		release["sha256"] = sha256hex
	}
	writeJSON(w, http.StatusOK, map[string]any{"release": release})
}

// FirmwareUpload handles POST /api/v1/admin/firmware (admin only).
// Accepts multipart form: version (string) + binary (file).
// Uploads to Tigris and sets the latest version in Redis.
func (a *App) FirmwareUpload(w http.ResponseWriter, r *http.Request) {
	if a.S3 == nil || a.Redis == nil {
		writeError(w, http.StatusServiceUnavailable, "S3 or Redis not configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxFirmwareSize)
	if err := r.ParseMultipartForm(maxFirmwareSize); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form or file too large")
		return
	}

	version := r.FormValue("version")
	if version == "" {
		writeError(w, http.StatusBadRequest, "version is required")
		return
	}

	file, _, err := r.FormFile("binary")
	if err != nil {
		writeError(w, http.StatusBadRequest, "binary file is required")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read binary")
		return
	}

	ctx := r.Context()
	key := s3.FirmwareKey(version)

	if err := a.S3.Upload(ctx, key, data, "application/octet-stream"); err != nil {
		slog.Error("firmware upload failed", "version", version, "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	hash := sha256.Sum256(data)
	sha256hex := hex.EncodeToString(hash[:])

	pipe := a.Redis.Pipeline()
	pipe.Set(ctx, "firmware:latest:version", version, 0)
	pipe.Set(ctx, "firmware:latest:sha256", sha256hex, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Error("firmware: failed to set latest version in Redis", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	meta, _ := json.Marshal(map[string]any{
		"version":    version,
		"s3_key":     key,
		"size_bytes": len(data),
		"sha256":     sha256hex,
	})
	a.Redis.Set(ctx, "firmware:latest:meta", meta, 0)

	slog.Info("firmware published", "version", version, "size_bytes", len(data), "sha256", sha256hex)
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    version,
		"size_bytes": len(data),
		"sha256":     sha256hex,
	})
}

// GithubWebhook handles POST /api/v1/webhooks/github.
func (a *App) GithubWebhook(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// QueryAudit handles GET /api/v1/audit.
func (a *App) QueryAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	eventType := r.URL.Query().Get("type")
	since := r.URL.Query().Get("since")
	until := r.URL.Query().Get("until")
	limit := parseQueryUint64(r, "limit", 50)
	offset := parseQueryUint64(r, "offset", 0)

	entries, total, err := a.DB.QueryAuditLog(ctx, eventType, since, until, int64(limit), int64(offset))
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   total,
	})
}
