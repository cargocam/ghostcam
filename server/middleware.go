package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/cargocam/ghostcam/server/auth"
)

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

// adminAuth wraps viewerAuth and enforces that the authenticated viewer's
// email matches AdminEmail. The admin email comes from the JWT claim that
// viewerAuth populated on the cookie path — admins must log in via the
// web UI, not via API token, because API-token auth does not populate
// KeyUserEmail (the token record doesn't carry an email).
func (a *App) adminAuth(next http.Handler) http.Handler {
	return a.viewerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := getUserID(r)
		if userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if getUserEmail(r) != a.Config.AdminEmail {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	}))
}

// cameraAuth authenticates cameras via Bearer API key.
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

		ctx := context.WithValue(r.Context(), keyCameraDeviceID, camera.DeviceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
