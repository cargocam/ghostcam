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

func (a *App) setAuthCookie(w http.ResponseWriter, r *http.Request, userID, email string) {
	// Stamp is_admin into the JWT as a UI hint. The adminAuth middleware
	// always re-checks the admins table on every admin request, so this
	// claim is never load-bearing for authorization — a stale or forged
	// claim can never grant elevated access, it only controls whether
	// the UI shows the admin panel button. A newly-granted admin sees
	// the hint on their next login; a revoked admin sees the stale hint
	// until their next login, and any admin call they try meanwhile
	// returns 403 from the middleware.
	isAdmin, err := a.DB.IsAdmin(r.Context(), userID)
	if err != nil {
		slog.Warn("setAuthCookie: is-admin lookup failed, treating as non-admin", "user_id", userID, "error", err)
		isAdmin = false
	}
	token := auth.SignJWT(userID, email, isAdmin, a.HMACSecret, jwtTTL)
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

	if user.DisabledAt != nil || user.DeletedAt != nil {
		auth.DummyVerify(body.Password)
		slog.Warn("login failed: account disabled or deleted",
			"email", body.Email, "ip", clientIP(r),
			"disabled", user.DisabledAt != nil, "deleted", user.DeletedAt != nil)
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

	a.setAuthCookie(w, r, user.UserID, user.Email)
	writeJSON(w, http.StatusOK, apitypes.LoginResponse{UserID: user.UserID})
}

// Register handles POST /api/v1/auth/register.
// Public registration is disabled — returns 403. Admin users are seeded on
// first run via GHOSTCAM_ADMIN_EMAIL / GHOSTCAM_ADMIN_PASSWORD env vars.
func (a *App) Register(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusForbidden, "registration_disabled")
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

	a.setAuthCookie(w, r, userID, getUserEmail(r))
	w.WriteHeader(http.StatusOK)
}
