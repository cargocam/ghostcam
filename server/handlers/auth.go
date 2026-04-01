package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cargocam/ghostcam/server/auth"
	"github.com/cargocam/ghostcam/server/ctxutil"
	"github.com/cargocam/ghostcam/server/db"
)

const jwtTTL = 30 * 24 * time.Hour // 30 days
const cookieMaxAge = 30 * 86400

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	UserID string `json:"user_id"`
}

func (h *Handlers) setAuthCookie(w http.ResponseWriter, userID string) {
	token := auth.SignJWT(userID, h.HMACSecret, jwtTTL)
	cookie := fmt.Sprintf("ghostcam-token=%s; Path=/; HttpOnly; SameSite=Strict; Max-Age=%d", token, cookieMaxAge)
	w.Header().Set("Set-Cookie", cookie)
}

// Login handles POST /api/v1/auth/login.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	var body loginRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(body.Password) > 128 {
		writeError(w, http.StatusBadRequest, "password must be 8-128 characters")
		return
	}

	user, err := h.DB.GetUserByEmail(r.Context(), body.Email)
	if err != nil {
		slog.Error("login: get user failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if user == nil {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	if user.DisabledAt != nil {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	ok, err := h.DB.VerifyPassword(r.Context(), user.UserID, body.Password)
	if err != nil || !ok {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	db.AuditLog("auth_success", "user_id", user.UserID)

	h.setAuthCookie(w, user.UserID)
	writeJSON(w, http.StatusOK, loginResponse{UserID: user.UserID})
}

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name,omitempty"`
}

type registerResponse struct {
	UserID string `json:"user_id"`
}

// Register handles POST /api/v1/auth/register.
func (h *Handlers) Register(w http.ResponseWriter, r *http.Request) {
	var body registerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if !strings.Contains(body.Email, "@") || len(body.Email) > 254 {
		writeError(w, http.StatusBadRequest, "invalid email address")
		return
	}

	if len(body.Password) < 8 || len(body.Password) > 128 {
		writeError(w, http.StatusBadRequest, "password must be 8-128 characters")
		return
	}

	existing, err := h.DB.GetUserByEmail(r.Context(), body.Email)
	if err != nil {
		slog.Error("register: check email failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "email already registered")
		return
	}

	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		slog.Error("register: hash password failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	displayName := body.DisplayName
	if displayName == "" {
		displayName = "User"
	}

	userID, err := h.DB.CreateUser(r.Context(), body.Email, hash, displayName)
	if err != nil {
		if strings.Contains(err.Error(), "23505") {
			writeError(w, http.StatusConflict, "email already registered")
			return
		}
		slog.Error("register: create user failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	h.setAuthCookie(w, userID)
	writeJSON(w, http.StatusCreated, registerResponse{UserID: userID})
}

// Logout handles POST /api/v1/auth/logout.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Set-Cookie", "ghostcam-token=; Path=/; HttpOnly; SameSite=Strict; Max-Age=0")
	w.WriteHeader(http.StatusOK)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword handles PATCH /api/v1/auth/password.
func (h *Handlers) ChangePassword(w http.ResponseWriter, r *http.Request) {
	userID := ctxutil.GetUserID(r)

	var body changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(body.NewPassword) < 8 || len(body.NewPassword) > 128 {
		writeError(w, http.StatusBadRequest, "password must be 8-128 characters")
		return
	}
	if len(body.CurrentPassword) > 128 {
		writeError(w, http.StatusBadRequest, "password must be 8-128 characters")
		return
	}

	ok, err := h.DB.VerifyPassword(r.Context(), userID, body.CurrentPassword)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	newHash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if err := h.DB.SetPassword(r.Context(), userID, newHash); err != nil {
		slog.Error("change password: set password failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Issue new JWT after password change
	h.setAuthCookie(w, userID)
	w.WriteHeader(http.StatusOK)
}
