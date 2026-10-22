package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/bus/memory"
	"github.com/sky-walkerx/trequila/internal/crypto"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/outbox"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/testdb"
	"github.com/sky-walkerx/trequila/internal/trips"
)

// callAsUser runs fn with a context carrying the authenticated user, the way the
// /mcp endpoint and the chat loop do: through the auth middleware.
func callAsUser(t *testing.T, a *auth.AuthService, token string, fn func(ctx context.Context)) {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.AuthMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { fn(r.Context()) })).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("auth middleware rejected the token: %d", rec.Code)
	}
}

func setup(t *testing.T, withKey bool) (*Server, *memory.Bus, *auth.AuthService, string) {
	t.Helper()
	db := testdb.New(t)
	t.Setenv("JWT_SECRET", strings.Repeat("k", 40))
	a := auth.NewAuthService(db)
	user, token, err := a.RegisterUser(context.Background(), "mcp@example.com", "a-long-password", "MCP")
	if err != nil {
		t.Fatal(err)
	}
	cipher, _ := crypto.NewCipher(make([]byte, 32))
	keys := providerkeys.NewStore(db, cipher)
	if withKey {
		if err := keys.Upsert(context.Background(), user.ID, "gemini", "key-12345", "", "gemini-2.5-flash", true); err != nil {
			t.Fatal(err)
		}
	}
	b := memory.New()
	store := &trips.Store{DB: db, Keys: keys, Relay: &outbox.Relay{DB: db, Pub: b}}
	return NewServer(db, store, keys, nil, nil, a), b, a, token
}

func toolRequest(args map[string]any) mcp.CallToolRequest {
	r := mcp.CallToolRequest{}
	r.Params.Arguments = args
	return r
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content %T", res.Content[0])
	}
	return tc.Text
}

func TestCreateTripStoresTheTripAndQueuesItsEvent(t *testing.T) {
	s, b, a, token := setup(t, true)
	var res *mcp.CallToolResult
	callAsUser(t, a, token, func(ctx context.Context) {
		res, _ = s.handleCreateTrip(ctx, toolRequest(map[string]any{
			"source": "Berlin", "destination": "Lisbon", "start_date": "2026-12-01", "end_date": "2026-12-03", "budget": 500.0,
		}))
	})

	if res.IsError {
		t.Fatalf("tool error: %s", resultText(t, res))
	}
	var out struct {
		TripID string `json:"trip_id"`
		Status string `json:"status"`
	}
	json.Unmarshal([]byte(resultText(t, res)), &out)
	if out.TripID == "" || out.Status != "pending" {
		t.Fatalf("result = %+v", out)
	}
	evs := b.Events()
	if len(evs) != 1 || evs[0].Type != events.TripRequested || evs[0].ID != events.ID(events.TripRequested, out.TripID) {
		t.Errorf("published = %+v", evs)
	}
}

func TestCreateTripReportsProblemsTheCallerCanFix(t *testing.T) {
	s, b, a, token := setup(t, true)
	cases := map[string]map[string]any{
		"bad date":    {"source": "A", "destination": "B", "start_date": "tomorrow", "end_date": "2026-12-03"},
		"reversed":    {"source": "A", "destination": "B", "start_date": "2026-12-05", "end_date": "2026-12-03"},
		"no location": {"source": "A", "start_date": "2026-12-01", "end_date": "2026-12-03"},
	}
	for name, args := range cases {
		var res *mcp.CallToolResult
		callAsUser(t, a, token, func(ctx context.Context) { res, _ = s.handleCreateTrip(ctx, toolRequest(args)) })
		if !res.IsError {
			t.Errorf("%s: expected a tool error, got %s", name, resultText(t, res))
		}
	}
	if len(b.Events()) != 0 {
		t.Errorf("invalid requests published %d events", len(b.Events()))
	}
}

func TestCreateTripWithoutAKeyTellsTheCallerToConfigureOne(t *testing.T) {
	s, _, a, token := setup(t, false)
	var res *mcp.CallToolResult
	callAsUser(t, a, token, func(ctx context.Context) {
		res, _ = s.handleCreateTrip(ctx, toolRequest(map[string]any{
			"source": "A", "destination": "B", "start_date": "2026-12-01", "end_date": "2026-12-03",
		}))
	})
	if !res.IsError || !strings.Contains(resultText(t, res), "API key") {
		t.Errorf("result = %+v", res)
	}
}

func TestCreateTripWithoutAnAuthenticatedUserIsRejected(t *testing.T) {
	s, _, _, _ := setup(t, true)
	res, _ := s.handleCreateTrip(context.Background(), toolRequest(map[string]any{}))
	if !res.IsError || !strings.Contains(resultText(t, res), "unauthorized") {
		t.Errorf("result = %+v", res)
	}
}
