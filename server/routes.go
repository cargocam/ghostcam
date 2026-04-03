package server

import (
	"net/http"
	"os"
	"strings"

	"github.com/cargocam/ghostcam/server/handlers"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// BuildRouter creates the chi router with all route groups.
func BuildRouter(app *App) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestSize(1 << 20)) // 1MB body limit
	r.Use(corsMiddleware(app.Config.PublicURL))

	secureCookies := strings.HasPrefix(app.Config.PublicURL, "https://")
	h := handlers.New(app.DB, app.Redis, app.S3, app.HMACSecret, app.Config.S3PresignTTLSecs, app.Config.AdminEmail, app.Config.PublicURL, secureCookies)

	// Rate limiters for public auth endpoints
	loginRL := NewRateLimiter(10)     // 10 req/min per IP
	registerRL := NewRateLimiter(5)   // 5 req/min per IP
	provisionRL := NewRateLimiter(10) // 10 req/min per IP

	// Public routes (no auth)
	r.Get("/healthz", h.Healthz)
	r.Get("/readyz", h.Readyz)
	r.With(loginRL.Middleware).Post("/api/v1/auth/login", h.Login)
	r.With(registerRL.Middleware).Post("/api/v1/auth/register", h.Register)
	r.With(provisionRL.Middleware).Post("/api/v1/cameras/provision", h.Provision)
	r.Get("/api/v1/billing/tiers", h.ListTiers)
	r.Get("/api/v1/firmware/latest", h.FirmwareLatest)
	r.Post("/api/v1/webhooks/stripe", h.StripeWebhook)
	r.Post("/api/v1/webhooks/github", h.GithubWebhook)

	// Camera-auth routes
	r.Group(func(r chi.Router) {
		r.Use(CameraAuth(app))
		r.Post("/api/v1/cameras/{deviceID}/presign", h.Presign)
		r.Post("/api/v1/cameras/{deviceID}/telemetry", h.PostTelemetry)
	})

	// Viewer-auth routes (session or API token)
	r.Group(func(r chi.Router) {
		r.Use(ViewerAuth(app))

		// Auth (protected)
		r.Post("/api/v1/auth/logout", h.Logout)
		r.Patch("/api/v1/auth/password", h.ChangePassword)

		// Cameras
		r.Get("/api/v1/cameras", h.ListCameras)
		r.Post("/api/v1/cameras", h.Enroll)
		r.Get("/api/v1/cameras/enroll/qr", h.EnrollmentQR)
		r.Post("/api/v1/cameras/enroll/qr", h.EnrollmentQR)
		r.Get("/api/v1/cameras/{deviceID}", h.GetCamera)
		r.Patch("/api/v1/cameras/{deviceID}", h.UpdateCamera)
		r.Delete("/api/v1/cameras/{deviceID}", h.DeleteCamera)

		// API tokens
		r.Get("/api/v1/tokens", h.ListTokens)
		r.Post("/api/v1/tokens", h.CreateToken)
		r.Delete("/api/v1/tokens/{tokenID}", h.RevokeToken)

		// Telemetry queries
		r.Get("/api/v1/telemetry/{deviceID}/latest", h.GetTelemetryLatest)
		r.Get("/api/v1/telemetry/{deviceID}", h.GetTelemetryRange)

		// SSE
		r.Get("/events", h.SSE)

		// HLS
		r.Get("/hls/{deviceID}/playlist.m3u8", h.GetManifest)
		r.Get("/hls/{deviceID}/init.mp4", h.GetInit)
		r.Get("/hls/{deviceID}/{segmentID}.ts", h.GetSegment)
		r.Get("/hls/{deviceID}/coverage", h.GetCoverage)

		// Billing
		r.Get("/api/v1/billing/subscription", h.GetSubscription)
		r.Post("/api/v1/billing/portal", h.CreatePortal)
		r.Get("/api/v1/billing/usage", h.GetUsage)

	})

	// Admin-only routes (viewer auth + admin email check)
	r.Group(func(r chi.Router) {
		r.Use(AdminAuth(app))
		r.Get("/api/v1/audit", h.QueryAudit)
		r.Post("/api/v1/admin/reload", h.ReloadConfig)
		r.Post("/api/v1/admin/firmware", h.FirmwareUpload)
	})

	// Static file serving (SPA fallback)
	staticDir := "/app/static"
	if dir := os.Getenv("GHOSTCAM_STATIC_DIR"); dir != "" {
		staticDir = dir
	}
	if info, err := os.Stat(staticDir); err == nil && info.IsDir() {
		fs := http.FileServer(http.Dir(staticDir))
		r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
			// Try serving the exact file first
			path := staticDir + req.URL.Path
			if _, err := os.Stat(path); err == nil {
				fs.ServeHTTP(w, req)
				return
			}
			// SPA fallback: serve index.html for non-file routes
			req.URL.Path = "/"
			fs.ServeHTTP(w, req)
		})
	}

	return r
}

// corsMiddleware adds CORS headers allowing the configured PublicURL origin
// and localhost:5173 for dev.
func corsMiddleware(publicURL string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool)
	allowed["http://localhost:5173"] = true
	if publicURL != "" {
		allowed[strings.TrimRight(publicURL, "/")] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Max-Age", "3600")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
