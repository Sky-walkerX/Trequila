package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postJSON(t *testing.T, mux *http.ServeMux, path string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("POST", path, bytes.NewReader(b)))
	return rr
}

func errorBody(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not valid JSON: %q (%v)", rr.Body.String(), err)
	}
	return out.Error
}

func TestRegisterAndLoginErrorsAreValidJSONAndDoNotLeak(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	mux := http.NewServeMux()
	srv.Routes(mux)

	if rr := postJSON(t, mux, "/auth/register", map[string]string{"email": "new@example.com", "password": "short"}); rr.Code != http.StatusBadRequest || errorBody(t, rr) == "" {
		t.Errorf("a short password: %d %q, want 400 with a JSON error", rr.Code, rr.Body.String())
	}

	if rr := postJSON(t, mux, "/auth/register", map[string]string{"email": "new@example.com", "password": "a-long-password", "name": "N"}); rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
	}

	unknown := postJSON(t, mux, "/auth/login", map[string]string{"email": "nobody@example.com", "password": "a-long-password"})
	wrong := postJSON(t, mux, "/auth/login", map[string]string{"email": "new@example.com", "password": "wrong-password"})
	if unknown.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("status unknown=%d wrong=%d, want 401 both", unknown.Code, wrong.Code)
	}
	if errorBody(t, unknown) != errorBody(t, wrong) {
		t.Errorf("an unknown email (%q) and a wrong password (%q) must be indistinguishable", errorBody(t, unknown), errorBody(t, wrong))
	}
	if strings.Contains(strings.ToLower(errorBody(t, unknown)), "not found") {
		t.Errorf("login confirms whether an email is registered: %q", errorBody(t, unknown))
	}
}
