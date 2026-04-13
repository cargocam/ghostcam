package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/auth"
	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/mailer"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/subscription"
)

// --- AdminListUsers ---

// AdminListUsers handles GET /api/v1/admin/users.
//
// Returns every user in the database (including soft-deleted ones) joined
// with admin status, subscription tier, and camera count. One query fans
// into the full list — no per-row secondary fetches. The tier display
// name is resolved at this level (not in the DB layer) because the
// billing cache lives on *App, not on *db.DB.
func (a *App) AdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.DB.ListAllUsers(r.Context())
	if err != nil {
		slog.Error("admin: list users failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	out := make([]apitypes.AdminUser, 0, len(users))
	for _, u := range users {
		out = append(out, a.toAPIUser(u))
	}
	writeJSON(w, http.StatusOK, apitypes.AdminListUsersResponse{Users: out})
}

// toAPIUser converts a db.AdminUserRecord to the wire-shape AdminUser,
// resolving the raw tier ID to a human-readable display name via the
// billing cache. Unknown tier IDs (archived prices, stale DB rows) fall
// through to the raw ID so admins can still spot drift.
func (a *App) toAPIUser(u db.AdminUserRecord) apitypes.AdminUser {
	tierName := "Free"
	if u.Tier != "" && u.Tier != billing.FreeTierID {
		if t, ok := a.Tiers.Get(u.Tier); ok {
			tierName = t.Name
		} else {
			tierName = u.Tier // unknown price ID — show the raw ID so the admin can debug
		}
	}
	return apitypes.AdminUser{
		UserID:      u.UserID,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		CreatedAt:   u.CreatedAt,
		VerifiedAt:  u.VerifiedAt,
		DisabledAt:  u.DisabledAt,
		DeletedAt:   u.DeletedAt,
		IsAdmin:     u.IsAdmin,
		Tier:        u.Tier,
		TierName:    tierName,
		CameraCount: u.CameraCount,
	}
}

// --- AdminCreateUser ---

// inviteTTL is the expiry for invite set-password tokens.
const inviteTTL = 24 * time.Hour

// AdminCreateUser handles POST /api/v1/admin/users.
//
// Inserts a new user on the free tier with an unusable placeholder
// password, then sends an invite email with a set-password link.
// The user cannot log in until they follow the link and choose a
// password. Public self-registration is still disabled — this is
// the only post-bootstrap user creation path.
func (a *App) AdminCreateUser(w http.ResponseWriter, r *http.Request) {
	var body apitypes.AdminCreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	email := strings.TrimSpace(strings.ToLower(body.Email))
	if !isLikelyEmail(email) {
		writeError(w, http.StatusBadRequest, "invalid_email")
		return
	}
	displayName := strings.TrimSpace(body.DisplayName)
	if displayName == "" {
		displayName = email
	}
	if len(displayName) > 128 {
		writeError(w, http.StatusBadRequest, "display_name_too_long")
		return
	}

	// Placeholder hash that Argon2id never produces — login is impossible
	// until the user sets a real password via the invite link.
	userID, err := a.DB.CreateUser(r.Context(), email, displayName, "!")
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "email_exists")
			return
		}
		slog.Error("admin: create user failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if err := a.DB.CreateSubscription(r.Context(), userID, "free", "active"); err != nil {
		slog.Warn("admin: create subscription for new user failed",
			"user_id", userID, "error", err)
	}

	slog.Info("audit", "event_type", "admin_user_invite",
		"actor", getUserEmail(r), "target_user_id", userID, "target_email", email)

	users, err := a.DB.ListAllUsers(r.Context())
	if err != nil {
		slog.Error("admin: post-create list failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	var created *apitypes.AdminUser
	for _, u := range users {
		if u.UserID == userID {
			apiUser := a.toAPIUser(u)
			created = &apiUser
			break
		}
	}
	if created == nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, apitypes.AdminCreateUserResponse{
		User: *created,
	})

	// Send invite email with a set-password link. Uses the password_reset
	// token purpose so the existing ResetPassword handler consumes it.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rawToken, err := a.DB.CreateEmailToken(ctx, a.HMACSecret, userID, "password_reset", nil, inviteTTL)
		if err != nil {
			slog.Error("admin invite: create token failed", "error", err)
			return
		}
		if err := a.Mailer.SendInvite(ctx, email, mailer.InviteData{
			DisplayName: displayName,
			Link:        rawToken,
		}); err != nil {
			slog.Error("admin invite: send email failed", "error", err)
		}
	}()
}

// --- AdminUpdateUser ---

// AdminUpdateUser handles PATCH /api/v1/admin/users/{userID}.
//
// Today the only supported mutation is toggling the disabled flag.
// Admins can disable other admins (belt-and-suspenders against a
// compromised account) but cannot disable themselves to prevent an
// accidental lock-out of the only operator. Soft-deleted users cannot
// be disabled or re-enabled — trashed is trashed until a dev hard
// deletes or manually unsets deleted_at.
func (a *App) AdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "userID")
	if targetID == "" {
		writeError(w, http.StatusBadRequest, "missing_user_id")
		return
	}

	var body apitypes.AdminUpdateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	target, err := a.DB.GetUserByID(r.Context(), targetID)
	if err != nil {
		slog.Error("admin: get target user failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "user_not_found")
		return
	}
	if target.DeletedAt != nil {
		writeError(w, http.StatusConflict, "user_deleted")
		return
	}

	if body.Disabled != nil {
		// Refuse to disable yourself — no self-lockout, same spirit
		// as the admin-demote-self guard.
		actorID := getUserID(r)
		if actorID == targetID && *body.Disabled {
			writeError(w, http.StatusForbidden, "self_disable_forbidden")
			return
		}
		if err := a.DB.SetUserDisabled(r.Context(), targetID, *body.Disabled); err != nil {
			slog.Error("admin: set user disabled failed", "error", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		slog.Info("audit", "event_type", "admin_user_disable",
			"actor", getUserEmail(r), "target_user_id", targetID,
			"disabled", *body.Disabled)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- AdminResetUserPassword ---

// AdminResetUserPassword handles POST /api/v1/admin/users/{userID}/reset-password.
//
// Generates a fresh random password, stores its hash, and returns the
// one-time plaintext. Works on any user — including other admins — so
// a compromised admin account can be recovered by a peer. Soft-deleted
// users cannot have their password reset (rejected with 409).
func (a *App) AdminResetUserPassword(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "userID")
	if targetID == "" {
		writeError(w, http.StatusBadRequest, "missing_user_id")
		return
	}

	target, err := a.DB.GetUserByID(r.Context(), targetID)
	if err != nil {
		slog.Error("admin: get target user failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "user_not_found")
		return
	}
	if target.DeletedAt != nil {
		writeError(w, http.StatusConflict, "user_deleted")
		return
	}

	password := auth.GenerateRandomPassword()
	hash, err := auth.HashPassword(password)
	if err != nil {
		slog.Error("admin: hash password failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if err := a.DB.SetPassword(r.Context(), targetID, hash); err != nil {
		slog.Error("admin: set password failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "admin_user_reset_password",
		"actor", getUserEmail(r), "target_user_id", targetID)

	writeJSON(w, http.StatusOK, apitypes.AdminResetPasswordResponse{
		GeneratedPassword: password,
	})
}

// --- AdminSoftDeleteUser ---

// AdminSoftDeleteUser handles DELETE /api/v1/admin/users/{userID}.
//
// Marks the user as soft-deleted and disabled, then cancels any active
// Stripe subscription so the customer stops being billed. The row
// remains in the database for audit purposes; hard deletion is a
// deliberate psql operation reserved for developers.
//
// Protections enforced:
//   - Cannot delete an admin. Remove their admins row via DB first, then
//     call this endpoint. This prevents the UI from nuking the only
//     operator by accident.
//   - Cannot delete yourself, regardless of admin status.
//   - Cannot re-delete an already-soft-deleted user.
func (a *App) AdminSoftDeleteUser(w http.ResponseWriter, r *http.Request) {
	targetID := chi.URLParam(r, "userID")
	if targetID == "" {
		writeError(w, http.StatusBadRequest, "missing_user_id")
		return
	}

	actorID := getUserID(r)
	if targetID == actorID {
		writeError(w, http.StatusForbidden, "self_delete_forbidden")
		return
	}

	target, err := a.DB.GetUserByID(r.Context(), targetID)
	if err != nil {
		slog.Error("admin: get target user failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "user_not_found")
		return
	}
	if target.DeletedAt != nil {
		writeError(w, http.StatusConflict, "already_deleted")
		return
	}

	// Admin-protection guard: refuse to soft-delete users who are
	// currently admins. They must be demoted via direct DB query first.
	isAdmin, err := a.DB.IsAdmin(r.Context(), targetID)
	if err != nil {
		slog.Error("admin: is-admin check failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if isAdmin {
		writeError(w, http.StatusForbidden, "cannot_delete_admin")
		return
	}

	// Cancel the user's Stripe subscription, if any, before marking
	// the row so the webhook-driven downgrade lands on a row that
	// still exists. We don't fail the delete if the cancel errors —
	// a Stripe outage shouldn't block admin operator cleanup.
	a.cancelStripeSubscriptionForUser(r.Context(), targetID)

	if err := a.DB.SoftDeleteUser(r.Context(), targetID); err != nil {
		slog.Error("admin: soft delete user failed", "error", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	slog.Info("audit", "event_type", "admin_user_soft_delete",
		"actor", getUserEmail(r), "target_user_id", targetID,
		"target_email", target.Email)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// cancelStripeSubscriptionForUser best-efforts cancels the user's active
// Stripe subscription on soft delete so they stop being billed. Errors
// are logged but not surfaced — the delete itself is the critical path.
func (a *App) cancelStripeSubscriptionForUser(ctx context.Context, userID string) {
	sub, err := a.DB.GetSubscription(ctx, userID)
	if err != nil || sub == nil || sub.StripeSubscriptionID == nil || *sub.StripeSubscriptionID == "" {
		return
	}
	stripe.Key = a.Config.StripeSecretKey
	if _, err := subscription.Cancel(*sub.StripeSubscriptionID, nil); err != nil {
		slog.Warn("admin: stripe cancel on soft delete failed",
			"user_id", userID, "subscription_id", *sub.StripeSubscriptionID, "error", err)
	}
}

// --- helpers ---

func isLikelyEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at >= len(s)-1 {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	if !strings.Contains(s[at+1:], ".") {
		return false
	}
	if len(s) > 254 {
		return false
	}
	return true
}

// isUniqueViolation returns true for pgx unique-constraint errors. Used
// by admin create-user to return 409 on duplicate email without leaking
// the index name to the client.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
