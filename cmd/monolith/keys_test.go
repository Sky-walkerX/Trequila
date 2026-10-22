package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"database/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/crypto"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
)

func setupTestServer(t *testing.T) (*Server, *sql.DB, string) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping API integration test: DATABASE_URL not set")
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("Failed to connect to db: %v", err)
	}

	// Create dummy user
	userID := "test-user-api-keys"
	db.Exec("INSERT INTO users (id, email, password_hash) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING", userID, "api@example.com", "hash")

	// Create keys store
	key := make([]byte, 32)
	c, _ := crypto.NewCipher(key)
	store := providerkeys.NewStore(db, c)

	srv := &Server{
		db:   db,
		auth: auth.NewAuthService(db),
		keys: store,
	}

	return srv, db, userID
}

func TestKeysAPI(t *testing.T) {
	srv, db, userID := setupTestServer(t)
	defer db.Close()
	defer db.Exec("DELETE FROM users WHERE id = $1", userID)

	handler := srv.setupRoutes() // Needs to include keys routes

	// Helper to make authorized requests
	doReq := func(method, path string, body interface{}) *httptest.ResponseRecorder {
		var req *http.Request
		if body != nil {
			b, _ := json.Marshal(body)
			req = httptest.NewRequest(method, path, bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}

		rr := httptest.NewRecorder()
		token, _ := srv.auth.GenerateToken(userID, "api@example.com", "Test User", "local")
		req.Header.Set("Authorization", "Bearer "+token)

		handler.ServeHTTP(rr, req)
		return rr
	}

	// 1. GET /api/keys (Empty)
	t.Run("GetEmptyKeys", func(t *testing.T) {
		srv.keys.Delete(context.Background(), userID, "gemini")
		rr := doReq("GET", "/api/keys", nil)
		if rr.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var keys []providerkeys.ProviderKey
		json.Unmarshal(rr.Body.Bytes(), &keys)
		if len(keys) != 0 {
			t.Errorf("Expected 0 keys, got %d", len(keys))
		}
	})

	// 2. POST /api/keys (Upsert)
	t.Run("PostKey", func(t *testing.T) {
		body := map[string]interface{}{
			"provider":      "gemini",
			"api_key":       "test-key-12345",
			"default_model": "gemini-1.5-flash",
			"make_default":  true,
		}
		rr := doReq("POST", "/api/keys", body)
		if rr.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	// 3. GET /api/keys (1 key)
	t.Run("GetOneKey", func(t *testing.T) {
		rr := doReq("GET", "/api/keys", nil)
		if rr.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rr.Code)
		}
		var keys []providerkeys.ProviderKey
		json.Unmarshal(rr.Body.Bytes(), &keys)
		if len(keys) != 1 {
			t.Fatalf("Expected 1 key, got %d", len(keys))
		}
		if keys[0].Provider != "gemini" || keys[0].MaskedKey != "sk-...2345" {
			t.Errorf("Unexpected key: %+v", keys[0])
		}
	})

	// 4. DELETE /api/keys/gemini
	t.Run("DeleteKey", func(t *testing.T) {
		rr := doReq("DELETE", "/api/keys/gemini", nil)
		if rr.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rr.Code)
		}

		rr2 := doReq("GET", "/api/keys", nil)
		var keys []providerkeys.ProviderKey
		json.Unmarshal(rr2.Body.Bytes(), &keys)
		if len(keys) != 0 {
			t.Errorf("Expected 0 keys after delete, got %d", len(keys))
		}
	})
}
