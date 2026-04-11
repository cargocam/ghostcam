// Ghostcam server. Packaged as `package main` so there is no wrapper under
// cmd/ — the server binary builds from this directory directly.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/cargocam/ghostcam/server/s3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	goredis "github.com/redis/go-redis/v9"
)

// App holds all shared server dependencies. Every handler is a method on
// *App so main.go can wire routes directly without any intermediate struct.
// Stripe configuration lives on Config — there is no separate copy here.
type App struct {
	Config     *ServerConfig
	DB         *db.DB
	Redis      *goredis.Client // nil if Redis not configured
	S3         *s3.Client      // nil if S3 not configured
	HMACSecret []byte
	Tiers      *billing.Cache
}

// stripeConfigured reports whether a Stripe secret key is set. Paid tier
// enforcement is skipped entirely when this is false (dev/local).
func (a *App) stripeConfigured() bool {
	return a.Config.StripeSecretKey != ""
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	database, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer database.Close()
	slog.Info("database connected")

	initialPassword, err := database.Initialize(ctx, cfg.AdminPassword, cfg.AdminEmail)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if initialPassword != "" {
		fmt.Println("============================================================")
		fmt.Println("Ghostcam server first run")
		fmt.Println()
		fmt.Printf("Admin email: %s\n", cfg.AdminEmail)
		fmt.Printf("Initial admin password: %s\n", initialPassword)
		fmt.Println()
		if cfg.AdminPassword == "" {
			fmt.Println("Log in and change this password.")
			fmt.Println()
		}
		fmt.Println("============================================================")
	}

	hmacSecret, err := database.GetHMACSecret(ctx)
	if err != nil {
		return fmt.Errorf("get HMAC secret: %w", err)
	}

	var redisClient *goredis.Client
	if cfg.RedisURL != "" {
		redisClient, err = redis.Connect(cfg.RedisURL)
		if err != nil {
			slog.Warn("redis connection failed (telemetry disabled)", "error", err)
		} else {
			defer redisClient.Close()
			slog.Info("redis connected")
		}
	} else {
		slog.Info("redis not configured, telemetry history disabled")
	}

	var s3Client *s3.Client
	if cfg.S3Bucket != "" {
		s3Client, err = s3.New(ctx, cfg.S3Bucket, cfg.S3Region, cfg.S3Endpoint, cfg.S3PresignTTLSecs)
		if err != nil {
			slog.Warn("S3 client init failed (segment uploads disabled)", "error", err)
		} else {
			slog.Info("S3/Tigris client initialized", "bucket", cfg.S3Bucket)
		}
	}

	tierCache := billing.NewCache()
	if cfg.StripeSecretKey != "" {
		// Refresh synchronously so handlers have tiers available for the
		// first request. Failure is logged but not fatal — the server
		// still starts and the cache repopulates on the next webhook
		// delivery (product/price.* events → RefreshTiers on StripeWebhook)
		// or when a user hits the settings Retry button, which calls the
		// authenticated POST /api/v1/billing/tiers/refresh endpoint.
		//
		// We deliberately do not run a background refresh ticker: the
		// server has no other long-running goroutines and billing is
		// not load-bearing enough to justify the exception.
		if err := tierCache.Refresh(ctx, cfg.StripeSecretKey); err != nil {
			slog.Warn("billing: initial tier cache refresh failed", "error", err)
		}
	}

	app := &App{
		Config:     cfg,
		DB:         database,
		Redis:      redisClient,
		S3:         s3Client,
		HMACSecret: hmacSecret,
		Tiers:      tierCache,
	}

	srv := &http.Server{
		Addr:         fmt.Sprintf("0.0.0.0:%d", cfg.HTTPPort),
		Handler:      app.router(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP server: %w", err)
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP shutdown error", "error", err)
	}

	slog.Info("goodbye")
	return nil
}

// router builds the chi router. Lives alongside main so handlers can be wired
// with a single pass through the file — no intermediate router builder.
func (a *App) router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestSize(50 << 20)) // 50MB body limit (firmware uploads)
	r.Use(corsMiddleware(a.Config.PublicURL))

	loginRL := NewRateLimiter(10)
	registerRL := NewRateLimiter(5)
	provisionRL := NewRateLimiter(10)
	clientLogRL := NewRateLimiter(60)
	tierRefreshRL := NewRateLimiter(5)

	// Public
	r.Get("/healthz", a.Healthz)
	r.Get("/readyz", a.Readyz)
	r.With(loginRL.Middleware).Post("/api/v1/auth/login", a.Login)
	r.With(registerRL.Middleware).Post("/api/v1/auth/register", a.Register)
	r.With(provisionRL.Middleware).Post("/api/v1/cameras/provision", a.Provision)
	r.Get("/api/v1/billing/tiers", a.ListTiers)
	r.Get("/api/v1/firmware/latest", a.FirmwareLatest)
	r.Post("/api/v1/webhooks/stripe", a.StripeWebhook)
	r.Post("/api/v1/webhooks/github", a.GithubWebhook)

	// Camera auth
	r.Group(func(r chi.Router) {
		r.Use(a.cameraAuth)
		r.Post("/api/v1/cameras/{deviceID}/presign", a.Presign)
		r.Post("/api/v1/cameras/{deviceID}/telemetry", a.PostTelemetry)
	})

	// Viewer auth (session or API token)
	r.Group(func(r chi.Router) {
		r.Use(a.viewerAuth)

		r.Post("/api/v1/auth/logout", a.Logout)
		r.Patch("/api/v1/auth/password", a.ChangePassword)

		r.Get("/api/v1/cameras", a.ListCameras)
		r.Post("/api/v1/cameras", a.Enroll)
		r.Get("/api/v1/cameras/enroll/qr", a.EnrollmentQR)
		r.Post("/api/v1/cameras/enroll/qr", a.EnrollmentQR)
		r.Get("/api/v1/cameras/{deviceID}", a.GetCamera)
		r.Patch("/api/v1/cameras/{deviceID}", a.UpdateCamera)
		r.Delete("/api/v1/cameras/{deviceID}", a.DeleteCamera)

		r.Get("/api/v1/tokens", a.ListTokens)
		r.Post("/api/v1/tokens", a.CreateToken)
		r.Delete("/api/v1/tokens/{tokenID}", a.RevokeToken)

		r.Get("/api/v1/telemetry/{deviceID}/latest", a.GetTelemetryLatest)
		r.Get("/api/v1/telemetry/{deviceID}", a.GetTelemetryRange)

		r.Get("/events", a.SSE)

		r.Post("/api/v1/clips/prepare", a.PrepareClip)
		r.Get("/api/v1/telemetry/{deviceID}/export", a.ExportTelemetry)

		r.Get("/api/v1/events", a.ListEvents)
		r.Get("/api/v1/events/unread", a.GetUnreadCount)
		r.Patch("/api/v1/events/{eventID}/read", a.MarkEventRead)
		r.Post("/api/v1/events/read-all", a.MarkAllEventsRead)
		r.Delete("/api/v1/events/{eventID}", a.DismissEvent)

		r.Get("/hls/{deviceID}/live.m3u8", a.GetLiveManifest)
		r.Get("/hls/{deviceID}/vod.m3u8", a.GetVodManifest)
		r.Get("/hls/{deviceID}/init.mp4", a.GetInit)
		r.Get("/hls/{deviceID}/{segmentID}.ts", a.GetSegment)
		r.Get("/hls/{deviceID}/coverage", a.GetCoverage)

		r.Get("/api/v1/billing/subscription", a.GetSubscription)
		r.Post("/api/v1/billing/checkout", a.CreateCheckout)
		r.Post("/api/v1/billing/portal", a.CreatePortal)
		r.Get("/api/v1/billing/usage", a.GetUsage)
		// Force a synchronous Stripe tier cache refresh. Public
		// /billing/tiers reads the cache only. This authenticated
		// variant is what the settings dialog's Retry button calls.
		r.With(tierRefreshRL.Middleware).Post("/api/v1/billing/tiers/refresh", a.RefreshTiers)

		// Client-side diagnostic logging. Off by default; the UI only
		// posts here when the "Client error logging" developer toggle
		// is enabled. Rate-limited separately to keep a buggy client
		// from flooding slog.
		r.With(clientLogRL.Middleware).Post("/api/v1/client-log", a.ClientLog)
	})

	// Admin
	r.Group(func(r chi.Router) {
		r.Use(a.adminAuth)
		r.Post("/api/v1/admin/firmware", a.FirmwareUpload)
		r.Get("/api/v1/admin/billing/tiers", a.AdminListBillingTiers)
		r.Post("/api/v1/admin/billing/tiers", a.AdminCreateBillingTier)
		r.Patch("/api/v1/admin/billing/tiers/{priceID}", a.AdminUpdateBillingTier)
		r.Post("/api/v1/admin/billing/tiers/{priceID}/archive", a.AdminArchiveBillingTier)
	})

	// Static SPA files
	staticDir := "/app/static"
	if dir := os.Getenv("GHOSTCAM_STATIC_DIR"); dir != "" {
		staticDir = dir
	}
	if info, err := os.Stat(staticDir); err == nil && info.IsDir() {
		fs := http.FileServer(http.Dir(staticDir))
		r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
			path := staticDir + req.URL.Path
			if _, err := os.Stat(path); err == nil {
				fs.ServeHTTP(w, req)
				return
			}
			req.URL.Path = "/"
			fs.ServeHTTP(w, req)
		})
	}

	return r
}

func corsMiddleware(publicURL string) func(http.Handler) http.Handler {
	allowed := map[string]bool{"http://localhost:5173": true}
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

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
