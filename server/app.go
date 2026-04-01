package server

import (
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/redis"
	"github.com/cargocam/ghostcam/server/s3"
)

// App holds all shared application state.
type App struct {
	DB         db.Database
	Redis      *redis.Client // nil if Redis not configured
	S3         *s3.Client    // nil if S3 not configured
	HMACSecret []byte
	Config     *ServerConfig
}

// NewApp creates a new App with the given dependencies.
func NewApp(database db.Database, redisClient *redis.Client, s3Client *s3.Client, hmacSecret []byte, config *ServerConfig) *App {
	return &App{
		DB:         database,
		Redis:      redisClient,
		S3:         s3Client,
		HMACSecret: hmacSecret,
		Config:     config,
	}
}
