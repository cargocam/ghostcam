package server

import (
	"net/http"

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

	h := handlers.New(app.DB, app.Redis, app.S3, app.HMACSecret, app.Config.S3PresignTTLSecs)

	// Public routes (no auth)
	r.Get("/healthz", h.Healthz)
	r.Get("/readyz", h.Readyz)
	r.Post("/api/v1/auth/login", h.Login)
	r.Post("/api/v1/auth/register", h.Register)
	r.Post("/api/v1/cameras/provision", h.Provision)
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
		r.Get("/hls/{deviceID}/coverage", h.GetCoverage)

		// Billing
		r.Get("/api/v1/billing/subscription", h.GetSubscription)
		r.Post("/api/v1/billing/portal", h.CreatePortal)
		r.Get("/api/v1/billing/usage", h.GetUsage)

		// Audit
		r.Get("/api/v1/audit", h.QueryAudit)

		// Admin
		r.Post("/api/v1/admin/reload", h.ReloadConfig)
	})

	return r
}
