package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/auth"
	"github.com/cargocam/ghostcam/server/mailer"
)

const (
	verifyEmailTTL   = 24 * time.Hour
	passwordResetTTL = 1 * time.Hour
	emailChangeTTL   = 24 * time.Hour
	loginOTPTTL      = 10 * time.Minute
)

// ForgotPassword handles POST /api/v1/auth/forgot-password.
// Always returns 200 to prevent email enumeration.
func (a *App) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var body apitypes.ForgotPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	if body.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	// Always return success to prevent enumeration
	w.WriteHeader(http.StatusOK)

	user, err := a.DB.GetUserByEmail(r.Context(), body.Email)
	if err != nil {
		slog.Error("forgot password: lookup failed", "error", err)
		return
	}
	if user == nil || user.DisabledAt != nil || user.DeletedAt != nil {
		return
	}

	a.DB.InvalidateEmailTokens(r.Context(), user.UserID, "password_reset")

	rawToken, err := a.DB.CreateEmailToken(r.Context(), a.HMACSecret, user.UserID, "password_reset", nil, passwordResetTTL)
	if err != nil {
		slog.Error("forgot password: create token failed", "error", err)
		return
	}

	if err := a.Mailer.SendPasswordReset(r.Context(), user.Email, mailer.PasswordResetData{
		DisplayName: user.DisplayName,
		Link:        rawToken,
	}); err != nil {
		slog.Error("forgot password: send email failed", "error", err)
	}
}

// ResetPassword handles POST /api/v1/auth/reset-password.
func (a *App) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var body apitypes.ResetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	if len(body.NewPassword) < 8 || len(body.NewPassword) > 128 {
		writeError(w, http.StatusBadRequest, "password must be 8-128 characters")
		return
	}

	rec, err := a.DB.ConsumeEmailToken(r.Context(), a.HMACSecret, body.Token, "password_reset")
	if err != nil {
		slog.Error("reset password: consume token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if rec == nil {
		writeError(w, http.StatusBadRequest, "invalid or expired token")
		return
	}

	newHash, err := auth.HashPassword(body.NewPassword)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if err := a.DB.SetPassword(r.Context(), rec.UserID, newHash); err != nil {
		slog.Error("reset password: set password failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "password_reset", "user_id", rec.UserID)

	user, _ := a.DB.GetUserByID(r.Context(), rec.UserID)
	if user != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a.Mailer.SendPasswordChanged(ctx, user.Email, mailer.PasswordChangedData{
				DisplayName: user.DisplayName,
			})
		}()
	}

	w.WriteHeader(http.StatusOK)
}

// VerifyEmail handles POST /api/v1/auth/verify-email.
func (a *App) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var body apitypes.VerifyEmailRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	rec, err := a.DB.ConsumeEmailToken(r.Context(), a.HMACSecret, body.Token, "verify_email")
	if err != nil {
		slog.Error("verify email: consume token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if rec == nil {
		writeError(w, http.StatusBadRequest, "invalid or expired token")
		return
	}

	if err := a.DB.MarkVerified(r.Context(), rec.UserID); err != nil {
		slog.Error("verify email: mark verified failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "email_verified", "user_id", rec.UserID)
	w.WriteHeader(http.StatusOK)
}

// ResendVerifyEmail handles POST /api/v1/auth/verify-email/resend.
// Authenticated — only for users who haven't verified yet.
func (a *App) ResendVerifyEmail(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)
	email := getUserEmail(r)

	user, err := a.DB.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if user.VerifiedAt != nil {
		writeError(w, http.StatusBadRequest, "email already verified")
		return
	}

	a.DB.InvalidateEmailTokens(r.Context(), userID, "verify_email")

	rawToken, err := a.DB.CreateEmailToken(r.Context(), a.HMACSecret, userID, "verify_email", nil, verifyEmailTTL)
	if err != nil {
		slog.Error("resend verify: create token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if err := a.Mailer.SendVerifyEmail(r.Context(), email, mailer.VerifyEmailData{
		DisplayName: user.DisplayName,
		Link:        rawToken,
	}); err != nil {
		slog.Error("resend verify: send email failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// RequestEmailChange handles PATCH /api/v1/auth/email.
// Requires current password for security. Sends confirmation to the new address.
func (a *App) RequestEmailChange(w http.ResponseWriter, r *http.Request) {
	userID := getUserID(r)

	var body apitypes.ChangeEmailRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	body.NewEmail = strings.TrimSpace(strings.ToLower(body.NewEmail))
	if body.NewEmail == "" {
		writeError(w, http.StatusBadRequest, "new_email is required")
		return
	}
	if body.CurrentPassword == "" || len(body.CurrentPassword) > 128 {
		writeError(w, http.StatusBadRequest, "current_password is required")
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

	// Check that the new email isn't already taken
	existing, err := a.DB.GetUserByEmail(r.Context(), body.NewEmail)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "email already in use")
		return
	}

	user, err := a.DB.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	a.DB.InvalidateEmailTokens(r.Context(), userID, "email_change")

	payload := body.NewEmail
	rawToken, err := a.DB.CreateEmailToken(r.Context(), a.HMACSecret, userID, "email_change", &payload, emailChangeTTL)
	if err != nil {
		slog.Error("request email change: create token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Send confirmation to the NEW address
	if err := a.Mailer.SendEmailChangeConfirm(r.Context(), body.NewEmail, mailer.EmailChangeConfirmData{
		DisplayName: user.DisplayName,
		NewEmail:    body.NewEmail,
		Link:        rawToken,
	}); err != nil {
		slog.Error("request email change: send email failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// ConfirmEmailChange handles POST /api/v1/auth/email/confirm.
func (a *App) ConfirmEmailChange(w http.ResponseWriter, r *http.Request) {
	var body apitypes.ConfirmEmailChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	rec, err := a.DB.ConsumeEmailToken(r.Context(), a.HMACSecret, body.Token, "email_change")
	if err != nil {
		slog.Error("confirm email change: consume token failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if rec == nil || rec.Payload == nil {
		writeError(w, http.StatusBadRequest, "invalid or expired token")
		return
	}

	newEmail := *rec.Payload

	// Check uniqueness again (another user could have claimed this email
	// between the change request and confirmation)
	existing, err := a.DB.GetUserByEmail(r.Context(), newEmail)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if existing != nil && existing.UserID != rec.UserID {
		writeError(w, http.StatusConflict, "email already in use")
		return
	}

	// Look up old email before updating
	user, _ := a.DB.GetUserByID(r.Context(), rec.UserID)
	oldEmail := ""
	displayName := ""
	if user != nil {
		oldEmail = user.Email
		displayName = user.DisplayName
	}

	if err := a.DB.SetEmail(r.Context(), rec.UserID, newEmail); err != nil {
		slog.Error("confirm email change: set email failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "email_changed", "user_id", rec.UserID, "old_email", oldEmail, "new_email", newEmail)

	// Notify old email about the change
	if oldEmail != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a.Mailer.SendEmailChangedNotice(ctx, oldEmail, mailer.EmailChangedNoticeData{
				DisplayName: displayName,
				NewEmail:    newEmail,
			})
		}()
	}

	w.WriteHeader(http.StatusOK)
}

// RequestLoginOTP handles POST /api/v1/auth/otp/request.
// Always returns 200 to prevent email enumeration.
func (a *App) RequestLoginOTP(w http.ResponseWriter, r *http.Request) {
	var body apitypes.RequestLoginOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	if body.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	// Always return success to prevent enumeration
	w.WriteHeader(http.StatusOK)

	user, err := a.DB.GetUserByEmail(r.Context(), body.Email)
	if err != nil {
		slog.Error("request OTP: lookup failed", "error", err)
		return
	}
	if user == nil || user.DisabledAt != nil || user.DeletedAt != nil {
		return
	}

	a.DB.InvalidateEmailTokens(r.Context(), user.UserID, "login_otp")

	code, err := a.DB.CreateEmailOTP(r.Context(), a.HMACSecret, user.UserID, "login_otp", loginOTPTTL)
	if err != nil {
		slog.Error("request OTP: create OTP failed", "error", err)
		return
	}

	if err := a.Mailer.SendLoginOTP(r.Context(), user.Email, mailer.LoginOTPData{
		Code: code,
	}); err != nil {
		slog.Error("request OTP: send email failed", "error", err)
	}
}

// VerifyLoginOTP handles POST /api/v1/auth/otp/verify.
func (a *App) VerifyLoginOTP(w http.ResponseWriter, r *http.Request) {
	var body apitypes.VerifyLoginOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	if body.Email == "" || body.Code == "" {
		writeError(w, http.StatusBadRequest, "email and code are required")
		return
	}

	user, err := a.DB.GetUserByEmail(r.Context(), body.Email)
	if err != nil {
		slog.Error("verify OTP: get user failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if user == nil {
		// Constant-time reject for unknown users
		auth.DummyVerify(body.Code)
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	if user.DisabledAt != nil || user.DeletedAt != nil {
		auth.DummyVerify(body.Code)
		slog.Warn("OTP login failed: account disabled or deleted",
			"email", body.Email, "ip", clientIP(r))
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	rec, err := a.DB.ConsumeEmailOTP(r.Context(), a.HMACSecret, user.UserID, body.Code)
	if err != nil {
		slog.Error("verify OTP: consume failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if rec == nil {
		slog.Warn("OTP login failed: invalid or expired code", "email", body.Email, "ip", clientIP(r))
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	slog.Info("audit", "event_type", "auth_success", "user_id", user.UserID, "auth_method", "otp")

	a.setAuthCookie(w, r, user.UserID, user.Email)
	writeJSON(w, http.StatusOK, apitypes.LoginResponse{UserID: user.UserID})
}
