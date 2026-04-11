package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/cargocam/ghostcam/server/auth"
)

// adminAuthDecision returns the HTTP status adminAuth should emit for a
// request given the result of each authorization check. Factored out as
// a pure function so the decision matrix can be unit-tested without
// spinning up an http.Handler or a DB.
//
//   - jwtValid=false          → 401 Unauthorized (bad/missing cookie)
//   - jwtValid, dbErr!=nil    → 500 Internal Server Error (admins table unreachable)
//   - jwtValid, !isAdmin      → 403 Forbidden
//   - jwtValid, isAdmin       → 200 OK (let the handler run)
func adminAuthDecision(jwtValid, isAdmin bool, dbErr error) int {
	if !jwtValid {
		return http.StatusUnauthorized
	}
	if dbErr != nil {
		return http.StatusInternalServerError
	}
	if !isAdmin {
		return http.StatusForbidden
	}
	return http.StatusOK
}

// errAdminCheck is returned by the decision function's DB-error path in
// tests. Kept unexported because it's a sentinel used only for the test
// table — production code surfaces pgx errors directly.
var errAdminCheck = errors.New("admin check failed")

// Context keys for values populated by middleware.
type contextKey string

const (
	keyUserID         contextKey = "user_id"
	keyUserEmail      contextKey = "user_email"
	keyCameraDeviceID contextKey = "camera_device_id"
)

func getUserID(r *http.Request) string {
	if v, ok := r.Context().Value(keyUserID).(string); ok {
		return v
	}
	return ""
}

func getUserEmail(r *http.Request) string {
	if v, ok := r.Context().Value(keyUserEmail).(string); ok {
		return v
	}
	return ""
}

func getCameraDeviceID(r *http.Request) string {
	if v, ok := r.Context().Value(keyCameraDeviceID).(string); ok {
		return v
	}
	return ""
}

// viewerAuth authenticates viewers via JWT cookie or Bearer API token.
func (a *App) viewerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// 1. Authorization: Bearer <api-token>
		// VerifyAPIToken already rejects expired tokens at the DB layer,
		// so no additional expiry check is needed here.
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			if token, ok := strings.CutPrefix(authHeader, "Bearer "); ok {
				tokenHash := auth.HMACToken(token, a.HMACSecret)
				record, err := a.DB.VerifyAPIToken(ctx, tokenHash)
				if err == nil && record != nil {
					ctx = context.WithValue(ctx, keyUserID, record.UserID)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
		}

		// 2. JWT cookie (stateless — no DB lookup)
		if cookie, err := r.Cookie("ghostcam-token"); err == nil {
			claims := auth.VerifyJWT(cookie.Value, a.HMACSecret)
			if claims != nil {
				ctx = context.WithValue(ctx, keyUserID, claims.UserID)
				ctx = context.WithValue(ctx, keyUserEmail, claims.Email)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}

// adminAuth gates /api/v1/admin/* on (1) a valid JWT cookie and (2) the
// authenticated user having a row in the admins table. It deliberately
// does NOT accept Bearer API tokens: admin actions are UI actions, and
// requiring cookie auth guarantees that keyUserEmail is populated (for
// audit log lines) and that admin CRUD never happens from a long-lived
// token that may have been exfiltrated.
//
// The admins-table check runs on every admin request. Admin traffic is
// low, and doing it live means grants and revocations take effect
// immediately — a revoked admin's stale JWT hint still hits 403 here,
// even though the UI may still render the admin panel button until
// their next login.
func (a *App) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cookie auth only: inline the JWT-cookie branch of viewerAuth
		// so we never accept a Bearer API token for admin scope.
		var (
			claims  *auth.JWTClaims
			isAdmin bool
			dbErr   error
		)
		if cookie, err := r.Cookie("ghostcam-token"); err == nil {
			claims = auth.VerifyJWT(cookie.Value, a.HMACSecret)
		}
		if claims != nil {
			isAdmin, dbErr = a.DB.IsAdmin(r.Context(), claims.UserID)
		}

		switch adminAuthDecision(claims != nil, isAdmin, dbErr) {
		case http.StatusUnauthorized:
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		case http.StatusInternalServerError:
			http.Error(w, "", http.StatusInternalServerError)
			return
		case http.StatusForbidden:
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		ctx := context.WithValue(r.Context(), keyUserID, claims.UserID)
		ctx = context.WithValue(ctx, keyUserEmail, claims.Email)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// cameraAuth authenticates cameras via Bearer API key and fails closed
// if the camera's owning user has been soft-deleted — segments must
// stop flowing the moment an admin trashes the account, even though
// the cameras table row still exists until a dev hard-deletes it.
func (a *App) cameraAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		token, ok := strings.CutPrefix(authHeader, "Bearer ")
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		tokenHash := auth.HMACToken(token, a.HMACSecret)
		camera, err := a.DB.GetCameraByAPIKey(r.Context(), tokenHash)
		if err != nil || camera == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Block the camera if its owner is soft-deleted. A missing
		// user row is also treated as deleted so hard-deleted orphans
		// fail closed. One extra DB hit per camera request is fine —
		// it's a primary-key lookup and cameras poll every ~10s.
		if camera.UserID != nil && *camera.UserID != "" {
			deleted, err := a.DB.IsUserDeleted(r.Context(), *camera.UserID)
			if err != nil {
				http.Error(w, "", http.StatusInternalServerError)
				return
			}
			if deleted {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}

		ctx := context.WithValue(r.Context(), keyCameraDeviceID, camera.DeviceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
