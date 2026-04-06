package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cargocam/ghostcam/server"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/cargocam/ghostcam/server/s3"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Configuration ---
	cfg, err := server.LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	// --- Database ---
	database, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer database.Close()
	slog.Info("database connected")

	// First-run initialization
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

	// --- HMAC secret ---
	hmacSecret, err := database.GetHMACSecret(ctx)
	if err != nil {
		return fmt.Errorf("get HMAC secret: %w", err)
	}

	// --- Redis (optional) ---
	var redisClient *redis.Client
	if cfg.RedisURL != "" {
		redisClient, err = redis.NewClient(cfg.RedisURL)
		if err != nil {
			slog.Warn("redis connection failed (telemetry disabled)", "error", err)
		} else {
			defer redisClient.Close()
			slog.Info("redis connected")
		}
	} else {
		slog.Info("redis not configured, telemetry history disabled")
	}

	// --- S3 / Tigris (optional) ---
	var s3Client *s3.Client
	if cfg.S3Bucket != "" {
		s3Client, err = s3.New(ctx, cfg.S3Bucket, cfg.S3Region, cfg.S3Endpoint, cfg.S3PresignTTLSecs)
		if err != nil {
			slog.Warn("S3 client init failed (segment uploads disabled)", "error", err)
		} else {
			slog.Info("S3/Tigris client initialized", "bucket", cfg.S3Bucket)
		}
	}

	// --- App state ---
	app := server.NewApp(database, redisClient, s3Client, hmacSecret, cfg)

	// --- Hourly session cleanup ---
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := database.CleanupExpiredSessions(ctx)
				if err != nil {
					slog.Warn("session cleanup failed", "error", err)
				} else if n > 0 {
					slog.Info("cleaned up expired sessions", "count", n)
				}
			}
		}
	}()

	// --- Hourly segment retention cleanup ---
	go func() {
		retentionDays := cfg.SegmentRetentionDays
		if retentionDays <= 0 {
			retentionDays = 30
		}
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cutoffMs := uint64(time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).UnixMilli())
				totalDeleted := 0
				for {
					deleted, err := database.DeleteOldSegments(ctx, cutoffMs, 100)
					if err != nil {
						slog.Warn("segment retention cleanup failed", "error", err)
						break
					}
					if len(deleted) == 0 {
						break
					}
					// Delete from S3
					if s3Client != nil {
						for _, seg := range deleted {
							if err := s3Client.Delete(ctx, seg.S3Key); err != nil {
								slog.Warn("failed to delete segment from S3", "s3_key", seg.S3Key, "error", err)
							}
						}
					}
					// Decrement cached storage counters per user
					if redisClient != nil {
						byDevice := make(map[string]int64)
						for _, seg := range deleted {
							byDevice[seg.DeviceID] += int64(seg.SizeBytes)
						}
						for devID, bytes := range byDevice {
							cam, _ := database.GetCamera(ctx, devID)
							if cam != nil && cam.UserID != nil {
								redisClient.RDB().DecrBy(ctx, "storage_bytes:"+*cam.UserID, bytes)
							}
						}
					}
					totalDeleted += len(deleted)
					if len(deleted) < 100 {
						break
					}
				}
				if totalDeleted > 0 {
					slog.Info("segment retention cleanup", "deleted", totalDeleted, "retention_days", retentionDays)
				}
			}
		}
	}()

	// --- Stale camera cleanup (every 6 hours) ---
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Delete unclaimed cameras older than 24 hours
				cutoff := time.Now().Add(-24 * time.Hour).Unix()
				n, err := database.DeleteStaleUnclaimedCameras(ctx, cutoff)
				if err != nil {
					slog.Warn("stale camera cleanup failed", "error", err)
				} else if n > 0 {
					slog.Info("cleaned up stale unclaimed cameras", "count", n)
				}

				// Delete expired provision tokens
				m, err := database.DeleteExpiredProvisionTokens(ctx)
				if err != nil {
					slog.Warn("provision token cleanup failed", "error", err)
				} else if m > 0 {
					slog.Info("cleaned up expired provision tokens", "count", m)
				}
			}
		}
	}()

	// --- HTTP server ---
	router := server.BuildRouter(app)
	httpAddr := fmt.Sprintf("0.0.0.0:%d", cfg.HTTPPort)

	srv := &http.Server{
		Addr:         httpAddr,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// --- Graceful shutdown ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("HTTP listening", "addr", httpAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			cancel()
		}
	}()

	<-sigCh
	slog.Info("shutting down")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP shutdown error", "error", err)
	}

	slog.Info("goodbye")
	return nil
}
