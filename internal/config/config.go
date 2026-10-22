// Package config reads the process configuration from the environment.
package config

import (
	"os"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/sky-walkerx/trequila/internal/middleware"
)

// Config holds everything the binary needs from its environment.
type Config struct {
	Port          string
	DatabaseURL   string
	RedisAddr     string
	EncryptionKey string
	FrontendURL   string // where OAuth callbacks send the browser back to

	NATSURL       string
	NATSCredsFile string // path to a .creds file (local)
	NATSCreds     string // inline .creds content (hosted platforms)

	LogLevel  string
	LogFormat string
}

// Load reads the environment. JWT_SECRET is validated by the auth service and
// ENCRYPTION_KEY by the crypto package, both of which refuse to start on a bad value.
func Load() Config {
	return Config{
		Port:          get("PORT", "8080"),
		DatabaseURL:   get("DATABASE_URL", get("DB_URL", "postgres://postgres:password@localhost:5432/trequila?sslmode=disable")),
		RedisAddr:     get("REDIS_URL", get("REDIS_ADDR", "localhost:6379")),
		EncryptionKey: os.Getenv("ENCRYPTION_KEY"),
		FrontendURL:   strings.TrimRight(get("FRONTEND_URL", middleware.DefaultAllowedOrigin), "/"),

		NATSURL:       get("NATS_URL", nats.DefaultURL),
		NATSCredsFile: os.Getenv("NATS_CREDS_FILE"),
		NATSCreds:     os.Getenv("NATS_CREDS"),

		LogLevel:  os.Getenv("LOG_LEVEL"),
		LogFormat: os.Getenv("LOG_FORMAT"),
	}
}

func get(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
