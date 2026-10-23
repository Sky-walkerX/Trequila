package trips_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/bus/memory"
	"github.com/sky-walkerx/trequila/internal/crypto"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/outbox"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/testdb"
	"github.com/sky-walkerx/trequila/internal/trips"
	"github.com/sky-walkerx/trequila/internal/types"
)

type env struct {
	db    *sql.DB
	store *trips.Store
	bus   *memory.Bus
}

func setup(t *testing.T) env {
	t.Helper()
	db := testdb.New(t)
	raw := make([]byte, 32)
	rand.Read(raw)
	cipher, err := crypto.NewCipherFromBase64(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	keys := providerkeys.NewStore(db, cipher)
	if _, err := db.Exec(`INSERT INTO users (id, email) VALUES ('u1', 'u1@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := keys.Upsert(context.Background(), "u1", "gemini", "secret-key", "", "gemini-2.5-flash", true); err != nil {
		t.Fatal(err)
	}
	b := memory.New()
	return env{db: db, bus: b, store: &trips.Store{DB: db, Keys: keys, Relay: &outbox.Relay{DB: db, Pub: b}}}
}

func params() trips.CreateParams {
	return trips.CreateParams{
		UserID: "u1", Source: "Berlin", Destination: "Lisbon",
		StartDate: "2026-12-01", EndDate: "2026-12-04", Budget: 900, Interests: []string{"food"},
	}
}

func TestCreateWritesTheTripItsOwnerAndTheEventTogether(t *testing.T) {
	e := setup(t)

	id, created, err := e.store.Create(context.Background(), params())
	if err != nil || !created {
		t.Fatalf("create = %q, %v, %v", id, created, err)
	}

	var status, provider, model string
	if err := e.db.QueryRow(`SELECT status, provider, model FROM trips WHERE request_id=$1`, id).Scan(&status, &provider, &model); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || provider != "gemini" || model != "gemini-2.5-flash" {
		t.Errorf("trip = %s %s %s", status, provider, model)
	}
	var role string
	if err := e.db.QueryRow(`SELECT role FROM trip_members WHERE trip_id=$1 AND user_id='u1'`, id).Scan(&role); err != nil || role != "owner" {
		t.Errorf("owner row = %q, %v", role, err)
	}

	// The kick published the event straight away.
	evs := e.bus.Events()
	if len(evs) != 1 || evs[0].Type != events.TripRequested || evs[0].ID != events.ID(events.TripRequested, id) {
		t.Fatalf("published events = %+v", evs)
	}
	var req types.TripRequest
	if err := evs[0].Decode(&req); err != nil {
		t.Fatal(err)
	}
	if req.RequestID != id || req.UserID != "u1" || req.Days != 4 || req.Provider != "gemini" || req.Destination != "Lisbon" {
		t.Errorf("payload = %+v", req)
	}
}

func TestAnEventThatCouldNotBePublishedStaysInTheOutboxForTheSweep(t *testing.T) {
	e := setup(t)
	e.store.Relay = &outbox.Relay{DB: e.db, Pub: failingPublisher{}}

	id, _, err := e.store.Create(context.Background(), params())
	if err != nil {
		t.Fatalf("a broker outage must not fail the request: %v", err)
	}
	var pending int
	e.db.QueryRow(`SELECT count(*) FROM outbox WHERE published_at IS NULL AND event_id=$1`, events.ID(events.TripRequested, id)).Scan(&pending)
	if pending != 1 {
		t.Errorf("pending outbox rows = %d, want 1", pending)
	}
}

type failingPublisher struct{}

func (failingPublisher) Publish(context.Context, bus.Event) error { return errors.New("broker down") }

func TestValidationErrors(t *testing.T) {
	e := setup(t)
	cases := map[string]func(*trips.CreateParams){
		"missing destination": func(p *trips.CreateParams) { p.Destination = "" },
		"bad start date":      func(p *trips.CreateParams) { p.StartDate = "01/12/2026" },
		"end before start":    func(p *trips.CreateParams) { p.EndDate = "2026-11-01" },
		"huge destination":    func(p *trips.CreateParams) { p.Destination = strings.Repeat("x", 5000) },
		"huge interest":       func(p *trips.CreateParams) { p.Interests = []string{strings.Repeat("x", 5000)} },
		"too many interests":  func(p *trips.CreateParams) { p.Interests = make([]string, 500) },
		"huge model name":     func(p *trips.CreateParams) { p.Model = strings.Repeat("m", 5000) },
		"negative days":       func(p *trips.CreateParams) { p.Days = -1 },
		"months-long trip":    func(p *trips.CreateParams) { p.EndDate = "2027-12-01" },
		"absurd day count":    func(p *trips.CreateParams) { p.Days = 100000 },
	}
	for name, mutate := range cases {
		p := params()
		mutate(&p)
		_, _, err := e.store.Create(context.Background(), p)
		var ve *trips.ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: got %v, want a ValidationError", name, err)
		}
	}
}

func TestNoKeyIsReportedAsErrNoKey(t *testing.T) {
	e := setup(t)
	p := params()
	p.UserID = "u2"
	e.db.Exec(`INSERT INTO users (id, email) VALUES ('u2', 'u2@example.com')`)

	_, _, err := e.store.Create(context.Background(), p)
	if !errors.Is(err, providerkeys.ErrNoKey) {
		t.Fatalf("got %v, want ErrNoKey", err)
	}
	var n int
	e.db.QueryRow(`SELECT count(*) FROM trips`).Scan(&n)
	if n != 0 {
		t.Errorf("a trip was created without a key: %d", n)
	}
}

func TestTheSameIdempotencyKeyAndRequestReturnsTheOriginalTrip(t *testing.T) {
	e := setup(t)
	p := params()
	p.IdempotencyKey = "abc"

	first, created1, err := e.store.Create(context.Background(), p)
	if err != nil || !created1 {
		t.Fatal(first, created1, err)
	}
	second, created2, err := e.store.Create(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if second != first || created2 {
		t.Errorf("second = %q created=%v, want %q created=false", second, created2, first)
	}
	var n int
	e.db.QueryRow(`SELECT count(*) FROM trips`).Scan(&n)
	if n != 1 || len(e.bus.Events()) != 1 {
		t.Errorf("trips=%d events=%d, want 1 each", n, len(e.bus.Events()))
	}
}

func TestReusingAnIdempotencyKeyForADifferentRequestIsRejected(t *testing.T) {
	e := setup(t)
	p := params()
	p.IdempotencyKey = "abc"
	if _, _, err := e.store.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.Destination = "Porto"

	_, _, err := e.store.Create(context.Background(), p)
	if !errors.Is(err, trips.ErrIdempotencyConflict) {
		t.Fatalf("got %v, want ErrIdempotencyConflict", err)
	}
}

func TestConcurrentRequestsWithOneKeyCreateOneTrip(t *testing.T) {
	e := setup(t)
	p := params()
	p.IdempotencyKey = "race"

	var wg sync.WaitGroup
	ids := make([]string, 6)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, _, err := e.store.Create(context.Background(), p)
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()

	var n int
	e.db.QueryRow(`SELECT count(*) FROM trips`).Scan(&n)
	if n != 1 {
		t.Fatalf("trips = %d, want 1", n)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Errorf("ids differ: %v", ids)
			break
		}
	}
}

func TestTheSameKeyFromAnotherUserIsIndependent(t *testing.T) {
	e := setup(t)
	e.db.Exec(`INSERT INTO users (id, email) VALUES ('u2', 'u2@example.com')`)
	keys := e.store.Keys
	keys.Upsert(context.Background(), "u2", "gemini", "other-secret", "", "gemini-2.5-flash", true)

	a := params()
	a.IdempotencyKey = "same"
	b := params()
	b.UserID = "u2"
	b.IdempotencyKey = "same"

	ida, _, err1 := e.store.Create(context.Background(), a)
	idb, _, err2 := e.store.Create(context.Background(), b)
	if err1 != nil || err2 != nil || ida == idb {
		t.Fatalf("ida=%q idb=%q errs=%v %v", ida, idb, err1, err2)
	}
}
