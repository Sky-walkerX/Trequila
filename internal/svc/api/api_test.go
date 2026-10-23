package api

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/bus/memory"
	"github.com/sky-walkerx/trequila/internal/crypto"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/outbox"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/testdb"
	"github.com/sky-walkerx/trequila/internal/tripevents"
	"github.com/sky-walkerx/trequila/internal/trips"
	"github.com/sky-walkerx/trequila/internal/types"
)

type harness struct {
	t      *testing.T
	svc    *Service
	db     *sql.DB
	bus    *memory.Bus
	srv    *httptest.Server
	token  string
	userID string
	subs   map[string]bus.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, url := testdb.NewWithURL(t)
	t.Setenv("JWT_SECRET", strings.Repeat("k", 40))

	authSvc := auth.NewAuthService(db)
	user, token, err := authSvc.RegisterUser(context.Background(), "owner@example.com", "a-long-password", "Owner")
	if err != nil {
		t.Fatal(err)
	}

	cipher, _ := crypto.NewCipher(make([]byte, 32))
	keys := providerkeys.NewStore(db, cipher)
	if err := keys.Upsert(context.Background(), user.ID, "gemini", "key-12345", "", "gemini-2.5-flash", true); err != nil {
		t.Fatal(err)
	}

	b := memory.New()
	relay := &outbox.Relay{DB: db, Pub: b}
	hub := tripevents.NewHub(url, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); hub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-hub.Ready()

	svc := New(Deps{
		DB: db, Auth: authSvc, Keys: keys, Hub: hub, Relay: relay,
		Trips: &trips.Store{DB: db, Keys: keys, Relay: relay},
	})
	svc.streamPlacesGrace = 400 * time.Millisecond
	svc.streamHardTimeout = 10 * time.Second
	svc.streamHeartbeat = 50 * time.Millisecond

	mux := http.NewServeMux()
	svc.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	h := &harness{t: t, svc: svc, db: db, bus: b, srv: srv, token: token, userID: user.ID, subs: map[string]bus.Handler{}}
	for _, s := range svc.Subscriptions() {
		h.subs[s.Type] = s.Handler
	}
	return h
}

func (h *harness) newTrip() string {
	h.t.Helper()
	id, _, err := h.svc.trips.Create(context.Background(), trips.CreateParams{
		UserID: h.userID, Source: "Berlin", Destination: "Lisbon", StartDate: "2026-12-01", EndDate: "2026-12-03",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// deliver feeds an event to the API's own consumer for that type, as the bus would.
func (h *harness) deliver(typ, id string, payload any) error {
	h.t.Helper()
	e, err := bus.NewEvent(typ, id, payload)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.subs[typ](context.Background(), e)
}

func (h *harness) deliverItinerary(tripID string) {
	h.t.Helper()
	err := h.deliver(events.TripItinerary, events.ID(events.TripItinerary, tripID), events.ItineraryGenerated{
		RequestID: tripID, Destination: "Lisbon",
		Itinerary: []types.DayPlan{{DayIndex: 0, Summary: "Alfama"}},
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) deliverPlaces(tripID string) {
	h.t.Helper()
	err := h.deliver(events.TripPlaces, events.ID(events.TripPlaces, tripID), events.PlacesFound{RequestID: tripID})
	if err != nil {
		h.t.Fatal(err)
	}
}

type sseEvent struct {
	ID    int64
	Name  string
	Data  string
	After time.Duration // since the request started
}

// stream reads an SSE response until the server closes it or max elapses.
func (h *harness) stream(tripID string, max time.Duration, header http.Header) ([]sseEvent, int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), max)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/api/trips/stream/"+tripID, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	for k, v := range header {
		req.Header[k] = v
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode
	}

	var out []sseEvent
	cur := sseEvent{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Name != "" {
				cur.After = time.Since(start)
				out = append(out, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			cur.ID, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out, 200
}

func names(evs []sseEvent) string {
	var n []string
	for _, e := range evs {
		n = append(n, e.Name)
	}
	return strings.Join(n, ",")
}

func TestARedeliveredResultIsAppliedOnce(t *testing.T) {
	h := newHarness(t)
	id := h.newTrip()

	h.deliverItinerary(id)
	h.deliverItinerary(id)
	h.deliverItinerary(id)

	var status string
	h.db.QueryRow(`SELECT status FROM trips WHERE request_id=$1`, id).Scan(&status)
	var itin, routeReq int
	h.db.QueryRow(`SELECT count(*) FROM trip_events WHERE trip_id=$1 AND type='itinerary'`, id).Scan(&itin)
	h.db.QueryRow(`SELECT count(*) FROM outbox WHERE type=$1`, events.TripRouteRequested).Scan(&routeReq)
	if status != "completed" || itin != 1 || routeReq != 1 {
		t.Errorf("status=%s itinerary events=%d route requests=%d; want completed,1,1", status, itin, routeReq)
	}
	// The kick published the route request.
	var found bool
	for _, e := range h.bus.Events() {
		if e.Type == events.TripRouteRequested {
			found = true
		}
	}
	if !found {
		t.Error("the route request was not published after the commit")
	}
}

func TestAResultForADeletedTripIsDroppedQuietly(t *testing.T) {
	h := newHarness(t)
	err := h.deliver(events.TripPlaces, "places:ghost", events.PlacesFound{RequestID: "no-such-trip"})
	if err != nil {
		t.Fatalf("want the event dropped without error, got %v", err)
	}
}

func TestStreamReplaysEverythingThenCompletes(t *testing.T) {
	h := newHarness(t)
	id := h.newTrip()
	h.deliverPlaces(id)
	h.deliverItinerary(id)

	evs, code := h.stream(id, 5*time.Second, nil)

	if code != 200 || names(evs) != "connected,places,itinerary,complete" {
		t.Fatalf("code=%d events=%s", code, names(evs))
	}
	if evs[1].ID == 0 || evs[2].ID <= evs[1].ID {
		t.Errorf("event IDs must be positive and increasing: %d, %d", evs[1].ID, evs[2].ID)
	}
	if evs[len(evs)-1].After > time.Second {
		t.Errorf("completion took %s; with everything already stored it should be immediate", evs[len(evs)-1].After)
	}
}

func TestEventsThatArriveWhileStreamingAreDeliveredAndCompleteTheStreamAtOnce(t *testing.T) {
	h := newHarness(t)
	id := h.newTrip()

	go func() {
		time.Sleep(150 * time.Millisecond)
		h.deliverItinerary(id)
		time.Sleep(100 * time.Millisecond)
		h.deliverPlaces(id)
	}()
	evs, _ := h.stream(id, 5*time.Second, nil)

	if names(evs) != "connected,itinerary,places,complete" {
		t.Fatalf("events = %s", names(evs))
	}
	if evs[len(evs)-1].After > 2*time.Second {
		t.Errorf("stream should complete as soon as places land, took %s", evs[len(evs)-1].After)
	}
}

func TestWithoutPlacesTheStreamCompletesAfterTheGracePeriod(t *testing.T) {
	h := newHarness(t)
	id := h.newTrip()
	h.deliverItinerary(id)

	evs, _ := h.stream(id, 5*time.Second, nil)

	if names(evs) != "connected,itinerary,complete" && !strings.HasSuffix(names(evs), ",complete") {
		t.Fatalf("events = %s", names(evs))
	}
	last := evs[len(evs)-1]
	if last.Name != "complete" || last.After < h.svc.streamPlacesGrace-50*time.Millisecond {
		t.Errorf("complete arrived after %s, before the %s grace", last.After, h.svc.streamPlacesGrace)
	}
	for _, e := range evs {
		if e.Name == "keepalive" {
			t.Error("comments must not surface as events")
		}
	}
}

func TestLastEventIDResumesWithoutRepeats(t *testing.T) {
	h := newHarness(t)
	id := h.newTrip()
	h.deliverPlaces(id)
	h.deliverItinerary(id)
	first, _ := h.stream(id, 5*time.Second, nil)
	placesID := first[1].ID
	h.svc.streamPlacesGrace = 3 * time.Second // a resume that forgot places would wait this long

	again, _ := h.stream(id, 5*time.Second, http.Header{"Last-Event-ID": {strconv.FormatInt(placesID, 10)}})

	// The client saw places before it dropped; the server must remember that, or it would
	// wait out the grace period for places that already arrived.
	if names(again) != "connected,itinerary,complete" {
		t.Fatalf("resumed events = %s, want connected,itinerary,complete", names(again))
	}
	if last := again[len(again)-1]; last.After > time.Second {
		t.Errorf("complete took %v; a resume must not wait for the places grace period", last.After)
	}
	for _, e := range again {
		if e.Name == "places" {
			t.Error("places was replayed even though the client had seen it")
		}
	}
}

func TestAFailedTripEndsWithFailedAndNoComplete(t *testing.T) {
	h := newHarness(t)
	id := h.newTrip()
	if err := h.deliver(events.TripFailed, "f1", events.Failed{RequestID: id, Stage: "planner", Reason: "API key invalid, expired, or out of credits."}); err != nil {
		t.Fatal(err)
	}

	evs, _ := h.stream(id, 5*time.Second, nil)

	if names(evs) != "connected,failed" {
		t.Fatalf("events = %s", names(evs))
	}
	var d struct{ Reason string }
	json.Unmarshal([]byte(evs[1].Data), &d)
	if !strings.Contains(d.Reason, "API key invalid") {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestAFailureForATripThatAlreadyCompletedChangesNothing(t *testing.T) {
	h := newHarness(t)
	id := h.newTrip()
	h.deliverItinerary(id)

	h.deliver(events.TripFailed, "late", events.Failed{RequestID: id, Reason: "too late"})

	var status string
	h.db.QueryRow(`SELECT status FROM trips WHERE request_id=$1`, id).Scan(&status)
	var failedEvents int
	h.db.QueryRow(`SELECT count(*) FROM trip_events WHERE trip_id=$1 AND type='failed'`, id).Scan(&failedEvents)
	if status != "completed" || failedEvents != 0 {
		t.Errorf("status=%s failed events=%d; a late failure must not undo a finished trip", status, failedEvents)
	}
}

func TestAnotherUsersTripCannotBeStreamed(t *testing.T) {
	h := newHarness(t)
	id := h.newTrip()
	h.db.Exec(`INSERT INTO users (id, email) VALUES ('intruder', 'intruder@example.com')`)
	tok, _ := h.svc.auth.GenerateToken("intruder", "intruder@example.com", "X", "local")
	h.token = tok

	_, code := h.stream(id, 2*time.Second, nil)

	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestCreatingATripOverHTTPHonoursIdempotencyKeys(t *testing.T) {
	h := newHarness(t)
	post := func(key, dest string) (*http.Response, map[string]string) {
		body, _ := json.Marshal(map[string]any{"source": "Berlin", "destination": dest, "start_date": "2026-12-01", "end_date": "2026-12-03"})
		req, _ := http.NewRequest("POST", h.srv.URL+"/api/trips", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}

	r1, b1 := post("key-1", "Lisbon")
	r2, b2 := post("key-1", "Lisbon")
	r3, _ := post("key-1", "Porto")
	r4, b4 := post("", "Lisbon")

	if r1.StatusCode != 202 || r2.StatusCode != 202 || b1["request_id"] == "" || b1["request_id"] != b2["request_id"] {
		t.Errorf("replay: %d %v / %d %v", r1.StatusCode, b1, r2.StatusCode, b2)
	}
	if r2.Header.Get("Idempotent-Replayed") != "true" || r1.Header.Get("Idempotent-Replayed") != "" {
		t.Errorf("replay header: first=%q second=%q", r1.Header.Get("Idempotent-Replayed"), r2.Header.Get("Idempotent-Replayed"))
	}
	if r3.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("conflicting reuse status = %d, want 422", r3.StatusCode)
	}
	if r4.StatusCode != 202 || b4["request_id"] == b1["request_id"] {
		t.Errorf("a request without a key must create a new trip: %d %v", r4.StatusCode, b4)
	}
	var n int
	h.db.QueryRow(`SELECT count(*) FROM trips`).Scan(&n)
	if n != 2 {
		t.Errorf("trips = %d, want 2", n)
	}
}

func TestOptimiseQueuesAFreshRouteRequestEachTime(t *testing.T) {
	h := newHarness(t)
	id := h.newTrip()
	h.deliverItinerary(id)

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("POST", h.srv.URL+"/api/trips/optimize/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+h.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 202 {
			t.Fatalf("optimise %d: %v %v", i, resp, err)
		}
		resp.Body.Close()
	}

	var n int
	h.db.QueryRow(`SELECT count(*) FROM outbox WHERE type=$1`, events.TripRouteRequested).Scan(&n)
	if n != 3 { // the itinerary's own request plus two explicit ones
		t.Errorf("route requests = %d, want 3", n)
	}
}

func TestStuckPendingTripsAreFailedAndAnnounced(t *testing.T) {
	h := newHarness(t)
	stuck := h.newTrip()
	fresh := h.newTrip()
	done := h.newTrip()
	h.deliverItinerary(done)
	h.db.Exec(`UPDATE trips SET created_at = now() - interval '30 minutes' WHERE request_id IN ($1, $2)`, stuck, done)

	n, err := h.svc.ReapStuck(context.Background(), 10*time.Minute)

	if err != nil || n != 1 {
		t.Fatalf("reaped %d, %v; want exactly the stuck trip", n, err)
	}
	status := func(id string) string {
		var s string
		h.db.QueryRow(`SELECT status FROM trips WHERE request_id=$1`, id).Scan(&s)
		return s
	}
	if status(stuck) != "failed" || status(fresh) != "pending" || status(done) != "completed" {
		t.Errorf("stuck=%s fresh=%s done=%s", status(stuck), status(fresh), status(done))
	}
	evs, _ := h.stream(stuck, 3*time.Second, nil)
	if names(evs) != "connected,failed" {
		t.Errorf("stream of the reaped trip = %s", names(evs))
	}
}

func TestMetricsAreOffUnlessATokenIsConfiguredAndThenRequireIt(t *testing.T) {
	h := newHarness(t)
	get := func(token string) int {
		req, _ := http.NewRequest("GET", h.srv.URL+"/metrics", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := get(""); got != http.StatusNotFound {
		t.Errorf("without METRICS_TOKEN configured, /metrics = %d, want 404", got)
	}

	// Rebuild the routes with a token.
	h.svc.metricsToken = "s3cret-scrape-token"
	mux := http.NewServeMux()
	h.svc.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	h.srv = srv

	if got := get(""); got != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", got)
	}
	if got := get("wrong"); got != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", got)
	}
	if got := get("s3cret-scrape-token"); got != http.StatusOK {
		t.Errorf("right token = %d, want 200", got)
	}
}
