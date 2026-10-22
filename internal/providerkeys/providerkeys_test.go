package providerkeys

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/crypto"
	"github.com/sky-walkerx/trequila/internal/testdb"
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db := testdb.New(t)
	userID := "test-user-providerkeys"
	if _, err := db.Exec("INSERT INTO users (id, email, password_hash) VALUES ($1, $2, $3)", userID, "test@example.com", "hash"); err != nil {
		t.Fatalf("Failed to insert dummy user: %v", err)
	}
	var err error

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
	if keys[0].Provider != "gemini" || keys[0].MaskedKey != "...-key" || !keys[0].IsDefault {
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

func TestUpsertRejectsBaseURLsThatReachInternalServices(t *testing.T) {
	db := testdb.New(t)
	if _, err := db.Exec("INSERT INTO users (id, email) VALUES ('u1', 'u1@example.com')"); err != nil {
		t.Fatal(err)
	}
	c, _ := crypto.NewCipher(make([]byte, 32))
	store := NewStore(db, c)
	ctx := context.Background()

	for _, bad := range []string{
		"http://api.example.com/v1",                // not https
		"https://169.254.169.254/latest/meta-data", // cloud metadata
		"https://127.0.0.1:8080",                   // loopback
		"https://10.0.0.7/v1",                      // private
		"https://metadata.google.internal",         // internal name
		"https://user:pw@api.example.com",          // credentials in the URL
	} {
		err := store.Upsert(ctx, "u1", "openai_compatible", "k", bad, "gpt-4o", true)
		if !errors.Is(err, ErrInvalidBaseURL) {
			t.Errorf("Upsert(%q) = %v, want ErrInvalidBaseURL", bad, err)
		}
	}
	var n int
	db.QueryRow("SELECT count(*) FROM provider_keys").Scan(&n)
	if n != 0 {
		t.Errorf("a rejected key was stored: %d rows", n)
	}

	if err := store.Upsert(ctx, "u1", "openai_compatible", "k", "https://api.openai.com/v1", "gpt-4o", true); err != nil {
		t.Errorf("a public https URL should be accepted: %v", err)
	}
}

func TestUpsertAllowsPrivateBaseURLsWhenTheOperatorOptsIn(t *testing.T) {
	t.Setenv("ALLOW_PRIVATE_PROVIDER_URLS", "true")
	db := testdb.New(t)
	db.Exec("INSERT INTO users (id, email) VALUES ('u1', 'u1@example.com')")
	c, _ := crypto.NewCipher(make([]byte, 32))

	err := NewStore(db, c).Upsert(context.Background(), "u1", "openai_compatible", "", "http://localhost:11434/v1", "llama3", true)
	if err != nil {
		t.Errorf("self-hosters running a local model must be able to opt in: %v", err)
	}
}
