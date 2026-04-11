package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/auth"
)

const jwtTTL = 30 * 24 * time.Hour // 30 days
const cookieMaxAge = 30 * 86400

func (a *App) setAuthCookie(w http.ResponseWriter, userID, email string) {
	// Admin status is resolved per-request by the adminAuth middleware
	// against the admins table — it is deliberately not stamped into
	// the JWT, so grants and revocations take effect immediately.
	token := auth.SignJWT(userID, email, a.HMACSecret, jwtTTL)
	secure := ""
	if a.Config.secureCookies() {
		secure = "; Secure"
	}
	// Not HttpOnly: the UI decodes the JWT client-side to derive the email
	// claim reactively for display. Auth still goes through the same cookie.
	cookie := fmt.Sprintf("ghostcam-token=%s; Path=/; SameSite=Strict%s; Max-Age=%d", token, secure, cookieMaxAge)
	w.Header().Set("Set-Cookie", cookie)
}

// Login handles POST /api/v1/auth/login.
func (a *App) Login(w http.ResponseWriter, r *http.Request) {
	var body apitypes.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(body.Password) > 128 {
		writeError(w, http.StatusBadRequest, "password must be 8-128 characters")
		return
	}

	user, err := a.DB.GetUserByEmail(r.Context(), body.Email)
	if err != nil {
		slog.Error("login: get user failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if user == nil {
		// Dummy password verification to equalize timing and prevent user
		// enumeration via response latency differences.
		auth.DummyVerify(body.Password)
		slog.Warn("login failed: unknown email", "email", body.Email, "ip", clientIP(r))
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	if user.DisabledAt != nil {
		auth.DummyVerify(body.Password)
		slog.Warn("login failed: account disabled", "email", body.Email, "ip", clientIP(r))
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	ok, err := a.DB.VerifyPassword(r.Context(), user.UserID, body.Password)
	if err != nil || !ok {
		slog.Warn("login failed: invalid password", "email", body.Email, "ip", clientIP(r))
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	slog.Info("audit", "event_type", "auth_success", "user_id", user.UserID)

	a.setAuthCookie(w, user.UserID, user.Email)
	writeJSON(w, http.StatusOK, apitypes.LoginResponse{UserID: user.UserID})
}

// Register handles POST /api/v1/auth/register.
// Public registration is disabled — returns 403. Admin users are seeded on
// first run via GHOSTCAM_ADMIN_EMAIL / GHOSTCAM_ADMIN_PASSWORD env vars.
func (a *App) Register(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusForbidden, "registration_disabled")
}

// Me handles GET /api/v1/auth/me. Returns the authenticated user's id,
// email, and (live, DB-resolved) admin status. The UI calls this to
// decide whether to render admin-only affordances — admin status is
// deliberately not in the JWT so grants/revocations take effect without
// a token rotation.
func (a *App) Me(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	email := getUserEmail(r)

	isAdmin, err := a.DB.IsAdmin(r.Context(), userID)
	if err != nil {
		slog.Error("me: is-admin check failed", "user_id", userID, "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, apitypes.AuthMeResponse{
		UserID:  userID,
		Email:   email,
		IsAdmin: isAdmin,
	})
}

// Logout handles POST /api/v1/auth/logout.
func (a *App) Logout(w http.ResponseWriter, _ *http.Request) {
	secure := ""
	if a.Config.secureCookies() {
		secure = "; Secure"
	}
	w.Header().Set("Set-Cookie", fmt.Sprintf("ghostcam-token=; Path=/; SameSite=Strict%s; Max-Age=0", secure))
	w.WriteHeader(http.StatusOK)
}

// ChangePassword handles PATCH /api/v1/auth/password.
func (a *App) ChangePassword(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body apitypes.ChangePasswordRequest
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

	ok, err := a.DB.VerifyPassword(r.Context(), userID, body.CurrentPassword)
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

	if err := a.DB.SetPassword(r.Context(), userID, newHash); err != nil {
		slog.Error("change password: set password failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	a.setAuthCookie(w, userID, getUserEmail(r))
	w.WriteHeader(http.StatusOK)
}
