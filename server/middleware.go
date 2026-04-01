package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/cargocam/ghostcam/server/auth"
	"github.com/cargocam/ghostcam/server/ctxutil"
)

// ViewerAuth is middleware that authenticates viewers via JWT cookie or Bearer API token.
func ViewerAuth(app *App) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			// 1. Check Authorization: Bearer <api-token>
			if authHeader := r.Header.Get("Authorization"); authHeader != "" {
				if token, ok := strings.CutPrefix(authHeader, "Bearer "); ok {
					tokenHash := auth.HMACToken(token, app.HMACSecret)
					record, err := app.DB.VerifyAPIToken(ctx, tokenHash)
					if err == nil && record != nil {
						now := time.Now().Unix()
						if record.ExpiresAt == nil || *record.ExpiresAt > now {
							ctx = context.WithValue(ctx, ctxutil.KeyUserID, record.UserID)
							next.ServeHTTP(w, r.WithContext(ctx))
							return
						}
					}
				}
			}

			// 2. Check JWT cookie (stateless — no DB lookup)
			if cookie, err := r.Cookie("ghostcam-token"); err == nil {
				userID := auth.VerifyJWT(cookie.Value, app.HMACSecret)
				if userID != "" {
					ctx = context.WithValue(ctx, ctxutil.KeyUserID, userID)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		})
	}
}

// CameraAuth is middleware that authenticates cameras via Bearer API key.
func CameraAuth(app *App) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
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

			tokenHash := auth.HMACToken(token, app.HMACSecret)
			camera, err := app.DB.GetCameraByAPIKey(r.Context(), tokenHash)
			if err != nil || camera == nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), ctxutil.KeyCameraDeviceID, camera.DeviceID)
			if camera.UserID != nil {
				ctx = context.WithValue(ctx, ctxutil.KeyCameraUserID, *camera.UserID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
