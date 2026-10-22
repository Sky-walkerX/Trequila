package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"database/sql"

	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/crypto"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/testdb"
)

func setupTestServer(t *testing.T) (*Service, *sql.DB, string) {
	db := testdb.New(t)
	t.Setenv("JWT_SECRET", strings.Repeat("k", 40))

	userID := "test-user-api-keys"
	if _, err := db.Exec("INSERT INTO users (id, email, password_hash) VALUES ($1, $2, $3)", userID, "api@example.com", "hash"); err != nil {
		t.Fatal(err)
	}

	c, err := crypto.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Deps{DB: db, Auth: auth.NewAuthService(db), Keys: providerkeys.NewStore(db, c)})
	return srv, db, userID
}

func TestKeysAPI(t *testing.T) {
	srv, _, userID := setupTestServer(t)

	mux := http.NewServeMux()
	srv.Routes(mux)
	var handler http.Handler = mux

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
		if keys[0].Provider != "gemini" || keys[0].MaskedKey != "...2345" {
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

func TestKeysAPIRefusesBaseURLsThatReachInternalServices(t *testing.T) {
	srv, _, userID := setupTestServer(t)
	mux := http.NewServeMux()
	srv.Routes(mux)
	token, _ := srv.auth.GenerateToken(userID, "api@example.com", "Test User", "local")

	post := func(path string, body map[string]any) (int, string) {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr.Code, rr.Body.String()
	}

	for _, target := range []string{"https://169.254.169.254/latest", "https://127.0.0.1:9", "http://example.com/v1", "https://10.0.0.1"} {
		code, body := post("/api/keys", map[string]any{"provider": "openai_compatible", "api_key": "k", "base_url": target, "default_model": "m"})
		if code != http.StatusBadRequest || !strings.Contains(body, "base_url") {
			t.Errorf("saving %s: %d %q, want 400 naming base_url", target, code, body)
		}
		code, body = post("/api/keys/test", map[string]any{"provider": "openai_compatible", "api_key": "k", "base_url": target, "model": "m"})
		if code != http.StatusBadRequest {
			t.Errorf("testing %s: %d %q, want 400", target, code, body)
		}
	}
}

func TestKeyTestDoesNotEchoWhatTheUpstreamSaid(t *testing.T) {
	t.Setenv("ALLOW_PRIVATE_PROVIDER_URLS", "true") // reach the fake upstream on loopback
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal secret: db password is hunter2", http.StatusBadRequest)
	}))
	defer upstream.Close()

	srv, _, userID := setupTestServer(t)
	mux := http.NewServeMux()
	srv.Routes(mux)
	token, _ := srv.auth.GenerateToken(userID, "api@example.com", "Test User", "local")

	b, _ := json.Marshal(map[string]any{"provider": "openai_compatible", "api_key": "k", "base_url": upstream.URL, "model": "m"})
	req := httptest.NewRequest("POST", "/api/keys/test", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if strings.Contains(rr.Body.String(), "hunter2") || strings.Contains(rr.Body.String(), "internal secret") {
		t.Errorf("the upstream's response leaked to the caller: %s", rr.Body.String())
	}
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
}
