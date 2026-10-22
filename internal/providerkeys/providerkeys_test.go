package providerkeys

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/crypto"
)

func TestMaskKey(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"sk-ant-api03-abcdefg1234567890", "...7890"},
		{"12345678", "...5678"},
		{"AIzaSyExample", "...mple"},
		{"abc", "***"},
		{"", "***"},
	}

	for _, tc := range tests {
		if got := MaskKey(tc.input); got != tc.want {
			t.Errorf("MaskKey(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestStore_Integration(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping integration test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Need a valid user in DB to satisfy foreign key.
	// We insert a dummy user and clean it up.
	userID := "test-user-providerkeys"
	_, err = db.Exec("INSERT INTO users (id, email, password_hash) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING", userID, "test@example.com", "hash")
	if err != nil {
		t.Fatalf("Failed to insert dummy user: %v", err)
	}
	defer func() {
		db.Exec("DELETE FROM users WHERE id = $1", userID)
	}()

	key := make([]byte, 32)
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("Failed to create cipher: %v", err)
	}

	store := NewStore(db, c)

	// Clean up any existing keys
	store.Delete(ctx, userID, "gemini")
	store.Delete(ctx, userID, "anthropic")

	// 1. Resolve with no keys should return ErrNoKey
	_, err = store.Resolve(ctx, userID, "")
	if err != ErrNoKey {
		t.Errorf("Expected ErrNoKey, got %v", err)
	}

	// 2. Upsert a key
	err = store.Upsert(ctx, userID, "gemini", "my-secret-key", "", "gemini-1.5-flash", true)
	if err != nil {
		t.Fatalf("Failed to upsert key: %v", err)
	}

	// 3. Resolve default key
	cfg, err := store.Resolve(ctx, userID, "")
	if err != nil {
		t.Fatalf("Failed to resolve key: %v", err)
	}
	if cfg.Kind != ai.ProviderGemini || cfg.APIKey != "my-secret-key" || cfg.Model != "gemini-1.5-flash" {
		t.Errorf("Unexpected resolved config: %+v", cfg)
	}

	// 4. List keys
	keys, err := store.List(ctx, userID)
	if err != nil {
		t.Fatalf("Failed to list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("Expected 1 key, got %d", len(keys))
	}
	if keys[0].Provider != "gemini" || keys[0].MaskedKey != "...t-key" || !keys[0].IsDefault {
		t.Errorf("Unexpected listed key: %+v", keys[0])
	}

	// 5. Upsert another key (non-default)
	err = store.Upsert(ctx, userID, "anthropic", "anthropic-key", "", "claude-3-haiku", false)
	if err != nil {
		t.Fatalf("Failed to upsert second key: %v", err)
	}

	// 6. Resolve override key
	cfg, err = store.Resolve(ctx, userID, "anthropic")
	if err != nil {
		t.Fatalf("Failed to resolve override key: %v", err)
	}
	if cfg.Kind != ai.ProviderAnthropic || cfg.APIKey != "anthropic-key" || cfg.Model != "claude-3-haiku" {
		t.Errorf("Unexpected resolved override config: %+v", cfg)
	}
}
