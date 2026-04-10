package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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
	secure := ""
	if h.SecureCookies {
		secure = "; Secure"
	}
	cookie := fmt.Sprintf("ghostcam-token=%s; Path=/; HttpOnly; SameSite=Strict%s; Max-Age=%d", token, secure, cookieMaxAge)
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
		// Perform a dummy password verification to equalize timing and prevent
		// user enumeration via response latency differences.
		auth.DummyVerify(body.Password)
		slog.Warn("login failed: unknown email", "email", body.Email, "ip", loginIP(r))
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	if user.DisabledAt != nil {
		auth.DummyVerify(body.Password)
		slog.Warn("login failed: account disabled", "email", body.Email, "ip", loginIP(r))
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	ok, err := h.DB.VerifyPassword(r.Context(), user.UserID, body.Password)
	if err != nil || !ok {
		slog.Warn("login failed: invalid password", "email", body.Email, "ip", loginIP(r))
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	db.AuditLog("auth_success", "user_id", user.UserID)

	h.setAuthCookie(w, user.UserID)
	writeJSON(w, http.StatusOK, loginResponse{UserID: user.UserID})
}

// Register handles POST /api/v1/auth/register.
// Public registration is disabled — returns 403. Admin users are seeded on
// first run via GHOSTCAM_ADMIN_EMAIL / GHOSTCAM_ADMIN_PASSWORD env vars.
// To re-enable registration, replace the body below with the original logic
// (see git history for the registerRequest flow).
func (h *Handlers) Register(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusForbidden, "registration_disabled")
}

// Logout handles POST /api/v1/auth/logout.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	secure := ""
	if h.SecureCookies {
		secure = "; Secure"
	}
	w.Header().Set("Set-Cookie", fmt.Sprintf("ghostcam-token=; Path=/; HttpOnly; SameSite=Strict%s; Max-Age=0", secure))
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

// loginIP extracts the client IP for login logging. Prefers Fly-Client-IP
// (trusted, set by Fly.io proxy) over X-Forwarded-For.
func loginIP(r *http.Request) string {
	if fci := r.Header.Get("Fly-Client-IP"); fci != "" {
		return fci
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	return r.RemoteAddr
}
