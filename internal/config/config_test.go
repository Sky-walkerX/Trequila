package config

import "testing"

func TestLoadAppliesDefaults(t *testing.T) {
	for _, k := range []string{"PORT", "NATS_URL", "DATABASE_URL", "DB_URL", "REDIS_URL", "REDIS_ADDR", "FRONTEND_URL", "LOG_LEVEL", "LOG_FORMAT"} {
		t.Setenv(k, "")
	}

	c := Load()

	if c.Port != "8080" || c.NATSURL != "nats://127.0.0.1:4222" || c.RedisAddr != "localhost:6379" {
		t.Errorf("defaults wrong: %+v", c)
	}
	if c.DatabaseURL == "" || c.FrontendURL == "" {
		t.Errorf("database and frontend defaults missing: %+v", c)
	}
}

func TestLoadReadsTheEnvironment(t *testing.T) {
	t.Setenv("PORT", "9000")
	t.Setenv("DATABASE_URL", "postgres://db/x")
	t.Setenv("REDIS_URL", "redis:6379")
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("FRONTEND_URL", "https://app.example.com/")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("ENCRYPTION_KEY", "abc")

	c := Load()

	if c.Port != "9000" || c.DatabaseURL != "postgres://db/x" || c.RedisAddr != "redis:6379" || c.NATSURL != "nats://nats:4222" {
		t.Errorf("env not read: %+v", c)
	}
	if c.FrontendURL != "https://app.example.com" {
		t.Errorf("FrontendURL = %q; the trailing slash should be trimmed", c.FrontendURL)
	}
	if c.LogLevel != "debug" || c.EncryptionKey != "abc" {
		t.Errorf("log level / key not read: %+v", c)
	}
}

func TestDatabaseURLFallsBackToTheLegacyName(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_URL", "postgres://legacy/x")
	if got := Load().DatabaseURL; got != "postgres://legacy/x" {
		t.Errorf("DatabaseURL = %q", got)
	}
}
