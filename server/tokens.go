package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/cargocam/ghostcam/server/auth"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type tokenResponse struct {
	TokenID    string `json:"token_id"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  *int64 `json:"expires_at,omitempty"`
	LastUsedAt *int64 `json:"last_used_at,omitempty"`
}

type createTokenRequest struct {
	Label     string `json:"label"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
}

type createTokenResponse struct {
	TokenID  string `json:"token_id"`
	RawToken string `json:"token"`
}

// ListTokens handles GET /api/v1/tokens.
func (a *App) ListTokens(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	tokens, err := a.DB.ListAPITokens(r.Context(), userID)
	if err != nil {
		slog.Error("list tokens failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	resp := make([]tokenResponse, 0, len(tokens))
	for _, t := range tokens {
		resp = append(resp, tokenResponse{
			TokenID:    t.TokenID,
			Label:      t.Label,
			CreatedAt:  t.CreatedAt,
			ExpiresAt:  t.ExpiresAt,
			LastUsedAt: t.LastUsedAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateToken handles POST /api/v1/tokens.
func (a *App) CreateToken(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Label == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}

	tokenID := uuid.New().String()
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	rawToken := base64.RawURLEncoding.EncodeToString(rawBytes)
	tokenHash := auth.HMACToken(rawToken, a.HMACSecret)

	err := a.DB.CreateAPIToken(r.Context(), &db.NewAPIToken{
		TokenID:   tokenID,
		UserID:    userID,
		TokenHash: tokenHash,
		Label:     body.Label,
		ExpiresAt: body.ExpiresAt,
	})
	if err != nil {
		slog.Error("create token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, createTokenResponse{
		TokenID:  tokenID,
		RawToken: rawToken,
	})
}

// RevokeToken handles DELETE /api/v1/tokens/{tokenID}.
func (a *App) RevokeToken(w http.ResponseWriter, r *http.Request) {
	tokenID := chi.URLParam(r, "tokenID")
	if err := a.DB.DeleteAPIToken(r.Context(), tokenID); err != nil {
		slog.Error("revoke token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
