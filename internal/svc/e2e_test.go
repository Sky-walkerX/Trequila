package svc_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/bus/memory"
	"github.com/sky-walkerx/trequila/internal/crypto"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/images"
	"github.com/sky-walkerx/trequila/internal/outbox"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/svc/api"
	"github.com/sky-walkerx/trequila/internal/svc/discovery"
	"github.com/sky-walkerx/trequila/internal/svc/planner"
	"github.com/sky-walkerx/trequila/internal/svc/router"
	"github.com/sky-walkerx/trequila/internal/testdb"
	"github.com/sky-walkerx/trequila/internal/tripevents"
	"github.com/sky-walkerx/trequila/internal/trips"
	"github.com/sky-walkerx/trequila/internal/types"
)

// --- fakes for the three things outside our control: the LLM and the open-data APIs ---

type fakeLLM struct {
	ai.LLMProvider
	plan []types.DayPlan
	err  error
	// hold, when set, delays the answer until it returns true. The pipeline runs the
	// planner and discovery side by side, so without it the route could be requested
	// before the places exist and the test would depend on which service is faster.
	hold func() bool
}

func (f fakeLLM) GenerateItinerary(ctx context.Context, _ types.TripRequest) ([]types.DayPlan, error) {
	for f.hold != nil && !f.hold() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return f.plan, f.err
}
func (f fakeLLM) Close() error { return nil }

type fakeGeo struct{}

func (fakeGeo) GetCoordinates(string) (float64, float64, error) { return 38.72, -9.14, nil }
func (fakeGeo) SearchPlaces(_, _ float64, category string, _ int) ([]geo.Place, error) {
	switch category {
	case "tourism.attraction":
		return []geo.Place{
			{Name: "Belem Tower", Lat: 38.6916, Lon: -9.2160},
			{Name: "Alfama", Lat: 38.7110, Lon: -9.1300},
		}, nil
	}
	return nil, nil
}

type fakeImages struct{}

func (fakeImages) GetImages(context.Context, string) ([]images.Image, error) {
	return []images.Image{{URL: "https://example.com/lisbon.jpg", Photographer: "Test"}}, nil
}

type noGeocoder struct{}

func (noGeocoder) GetCoordinates(string) (float64, float64, error) {
	return 0, 0, errors.New("offline")
}

// --- the system under test ---

type system struct {
	t      *testing.T
	db     *sql.DB
	srv    *httptest.Server
	bus    *memory.Bus
	token  string
	userID string
}

func start(t *testing.T, llm fakeLLM) *system {
	t.Helper()
	db, url := testdb.NewWithURL(t)
	t.Setenv("JWT_SECRET", strings.Repeat("k", 40))

	authSvc := auth.NewAuthService(db)
	cipher, _ := crypto.NewCipher(make([]byte, 32))
	keys := providerkeys.NewStore(db, cipher)
	b := memory.New()
	relay := &outbox.Relay{DB: db, Pub: b}
	hub := tripevents.NewHub(url, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	var done []chan struct{}
	run := func(fn func()) {
		c := make(chan struct{})
		done = append(done, c)
		go func() { defer close(c); fn() }()
	}
	t.Cleanup(func() {
		cancel()
		for _, c := range done {
			<-c
		}
	})
	run(func() { hub.Run(ctx) })
	<-hub.Ready()

	apiSvc := api.New(api.Deps{
		DB: db, Auth: authSvc, Keys: keys, Hub: hub, Relay: relay,
		Trips: &trips.Store{DB: db, Keys: keys, Relay: relay},
	})
	plan := planner.New(keys, b, planner.WithProviderFactory(func(context.Context, ai.ProviderConfig) (ai.LLMProvider, error) {
		return llm, nil
	}))

	subs := append([]bus.Subscription{}, apiSvc.Subscriptions()...)
	subs = append(subs, plan.Subscriptions()...)
	subs = append(subs, discovery.New(fakeGeo{}, fakeImages{}, b).Subscriptions()...)
	subs = append(subs, router.New(noGeocoder{}, b).Subscriptions()...)
	for _, s := range subs {
		s := s
		run(func() { b.Subscribe(ctx, s) })
	}

	mux := http.NewServeMux()
	apiSvc.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := &system{t: t, db: db, srv: srv, bus: b}
	s.register()
	return s
}

func (s *system) do(method, path string, body any) (*http.Response, []byte) {
	s.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, s.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func (s *system) register() {
	s.t.Helper()
	resp, body := s.do("POST", "/auth/register", map[string]string{"email": "e2e@example.com", "password": "a-long-password", "name": "E2E"})
	if resp.StatusCode != 201 {
		s.t.Fatalf("register: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Token string
		User  struct{ ID string }
	}
	json.Unmarshal(body, &out)
	s.token, s.userID = out.Token, out.User.ID

	resp, body = s.do("POST", "/api/keys", map[string]any{"provider": "gemini", "api_key": "e2e-key-12345", "default_model": "gemini-2.5-flash", "make_default": true})
	if resp.StatusCode != 200 {
		s.t.Fatalf("store key: %d %s", resp.StatusCode, body)
	}
}

func (s *system) createTrip() string {
	s.t.Helper()
	resp, body := s.do("POST", "/api/trips", map[string]any{
		"source": "Berlin", "destination": "Lisbon", "start_date": "2026-12-01", "end_date": "2026-12-02", "interests": []string{"food"},
	})
	if resp.StatusCode != 202 {
		s.t.Fatalf("create trip: %d %s", resp.StatusCode, body)
	}
	var out struct {
		RequestID string `json:"request_id"`
	}
	json.Unmarshal(body, &out)
	return out.RequestID
}

type sse struct{ Name, Data string }

func (s *system) stream(tripID string) []sse {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.srv.URL+"/api/trips/stream/"+tripID, nil)
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []sse
	var cur sse
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Name != "" {
				out = append(out, cur)
			}
			cur = sse{}
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

func (s *system) eventually(what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %s", what)
}

func has(evs []sse, name string) bool {
	for _, e := range evs {
		if e.Name == name {
			return true
		}
	}
	return false
}

func TestATripGoesFromRequestToCompletedAcrossAllServices(t *testing.T) {
	plan := []types.DayPlan{
		{DayIndex: 0, Date: "2026-12-01", Summary: "Belem", Activities: []types.Activity{{Name: "Belem Tower", Time: "10:00"}, {Name: "Alfama", Time: "15:00"}}},
		{DayIndex: 1, Date: "2026-12-02", Summary: "Old town", Activities: []types.Activity{{Name: "Alfama", Time: "10:00"}}},
	}
	var sys atomic.Pointer[system]
	s := start(t, fakeLLM{plan: plan, hold: func() bool {
		cur := sys.Load()
		if cur == nil {
			return false
		}
		var n int
		cur.db.QueryRow(`SELECT count(*) FROM trip_events WHERE type = 'places'`).Scan(&n)
		return n > 0
	}})
	sys.Store(s)

	id := s.createTrip()
	evs := s.stream(id)

	if !has(evs, "itinerary") || !has(evs, "places") || evs[len(evs)-1].Name != "complete" || has(evs, "failed") {
		t.Fatalf("stream = %+v", evs)
	}
	var it struct{ Itinerary []types.DayPlan }
	for _, e := range evs {
		if e.Name == "itinerary" {
			json.Unmarshal([]byte(e.Data), &it)
		}
	}
	if len(it.Itinerary) != 2 || it.Itinerary[0].Summary != "Belem" {
		t.Errorf("itinerary frame = %+v", it)
	}

	// Images and the route finish independently of completion; wait for them to land.
	s.eventually("images, places, itinerary and route to be stored", func() bool {
		var n int
		s.db.QueryRow(`SELECT count(*) FROM trips WHERE request_id=$1 AND status='completed'
			AND images IS NOT NULL AND places IS NOT NULL AND itinerary IS NOT NULL AND route IS NOT NULL`, id).Scan(&n)
		return n == 1
	})

	// Exactly one log entry of each kind, despite at-least-once delivery everywhere.
	rows, _ := s.db.Query(`SELECT type, count(*) FROM trip_events WHERE trip_id=$1 GROUP BY type`, id)
	counts := map[string]int{}
	for rows.Next() {
		var typ string
		var n int
		rows.Scan(&typ, &n)
		counts[typ] = n
	}
	rows.Close()
	for _, typ := range []string{"images", "places", "itinerary", "route"} {
		if counts[typ] != 1 {
			t.Errorf("trip_events %s = %d, want 1 (all: %v)", typ, counts[typ], counts)
		}
	}

	// The stored route is built from the places discovery found, with no geocoding.
	var routeJSON []byte
	s.db.QueryRow(`SELECT route FROM trips WHERE request_id=$1`, id).Scan(&routeJSON)
	var route struct {
		Days []struct {
			Ordered []struct{ Name string }
		}
	}
	json.Unmarshal(routeJSON, &route)
	if len(route.Days) != 2 || len(route.Days[0].Ordered) != 2 {
		t.Errorf("route = %s", routeJSON)
	}

	// A second stream of the finished trip replays the same history and completes.
	again := s.stream(id)
	if again[len(again)-1].Name != "complete" || !has(again, "route") {
		t.Errorf("replay = %+v", again)
	}
}

func TestARejectedKeyFailsTheTripAndTheStreamSaysSo(t *testing.T) {
	s := start(t, fakeLLM{err: errors.New("403 permission denied")})

	id := s.createTrip()
	evs := s.stream(id)

	last := evs[len(evs)-1]
	if last.Name != "failed" || has(evs, "complete") {
		t.Fatalf("stream = %+v", evs)
	}
	if !strings.Contains(last.Data, "API key invalid") {
		t.Errorf("failure data = %s", last.Data)
	}
	var status, reason string
	s.db.QueryRow(`SELECT status, error_reason FROM trips WHERE request_id=$1`, id).Scan(&status, &reason)
	if status != "failed" || !strings.Contains(reason, "API key invalid") {
		t.Errorf("status=%s reason=%q", status, reason)
	}
}
