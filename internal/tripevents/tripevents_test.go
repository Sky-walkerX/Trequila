package tripevents_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/sky-walkerx/trequila/internal/testdb"
	"github.com/sky-walkerx/trequila/internal/tripevents"
)

func seedTrip(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	db.Exec(`INSERT INTO users (id, email) VALUES ('u1', 'u1@example.com') ON CONFLICT DO NOTHING`)
	if _, err := db.Exec(`INSERT INTO trips (request_id, user_id, source, destination, start_date, end_date)
		VALUES ($1, 'u1', 'A', 'B', '2026-01-01', '2026-01-02')`, id); err != nil {
		t.Fatal(err)
	}
}

func appendCommitted(t *testing.T, db *sql.DB, tripID, typ string, data any) int64 {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	id, err := tripevents.Append(context.Background(), tx, tripID, typ, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAfterReturnsOneTripsEventsInOrderPastTheCursor(t *testing.T) {
	db := testdb.New(t)
	seedTrip(t, db, "t1")
	seedTrip(t, db, "t2")
	a := appendCommitted(t, db, "t1", "images", map[string]any{"n": 1})
	appendCommitted(t, db, "t2", "images", map[string]any{"n": 99})
	b := appendCommitted(t, db, "t1", "places", map[string]any{"n": 2})

	all, err := tripevents.After(context.Background(), db, "t1", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("after 0 = %v, %v", all, err)
	}
	if all[0].ID != a || all[1].ID != b || all[0].Type != "images" || all[1].Type != "places" {
		t.Errorf("order/contents wrong: %+v", all)
	}

	rest, _ := tripevents.After(context.Background(), db, "t1", a)
	if len(rest) != 1 || rest[0].ID != b {
		t.Errorf("after cursor = %+v, want only the second event", rest)
	}
	var data map[string]int
	if err := json.Unmarshal(rest[0].Data, &data); err != nil || data["n"] != 2 {
		t.Errorf("data = %s", rest[0].Data)
	}
}

func startHub(t *testing.T, url string) *tripevents.Hub {
	t.Helper()
	h := tripevents.NewHub(url, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-h.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("hub never started listening")
	}
	return h
}

func rang(ch <-chan struct{}, within time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

func TestACommittedEventRingsTheTripsDoorbell(t *testing.T) {
	db, url := testdb.NewWithURL(t)
	seedTrip(t, db, "t1")
	h := startHub(t, url)
	bell, cancel := h.Subscribe("t1")
	defer cancel()

	appendCommitted(t, db, "t1", "images", map[string]any{})

	if !rang(bell, 5*time.Second) {
		t.Fatal("doorbell did not ring")
	}
}

func TestARolledBackEventDoesNotRing(t *testing.T) {
	db, url := testdb.NewWithURL(t)
	seedTrip(t, db, "t1")
	h := startHub(t, url)
	bell, cancel := h.Subscribe("t1")
	defer cancel()

	tx, _ := db.Begin()
	tripevents.Append(context.Background(), tx, "t1", "images", map[string]any{})
	tx.Rollback()

	if rang(bell, 400*time.Millisecond) {
		t.Fatal("doorbell rang for a rolled-back event")
	}
}

func TestDoorbellsAreIndependentPerTripAndStopAfterCancel(t *testing.T) {
	db, url := testdb.NewWithURL(t)
	seedTrip(t, db, "t1")
	seedTrip(t, db, "t2")
	h := startHub(t, url)
	b1, cancel1 := h.Subscribe("t1")
	b2, cancel2 := h.Subscribe("t2")
	defer cancel2()

	appendCommitted(t, db, "t1", "images", map[string]any{})
	if !rang(b1, 5*time.Second) {
		t.Fatal("t1 doorbell did not ring")
	}
	if rang(b2, 300*time.Millisecond) {
		t.Fatal("t2 doorbell rang for t1's event")
	}

	cancel1()
	appendCommitted(t, db, "t1", "places", map[string]any{})
	if rang(b1, 300*time.Millisecond) {
		t.Fatal("cancelled subscription still rang")
	}
}

func TestSeveralEventsBeforeTheReaderWakesCoalesceIntoOneRing(t *testing.T) {
	db, url := testdb.NewWithURL(t)
	seedTrip(t, db, "t1")
	h := startHub(t, url)
	bell, cancel := h.Subscribe("t1")
	defer cancel()

	for i := 0; i < 5; i++ {
		appendCommitted(t, db, "t1", "images", map[string]any{"i": i})
	}
	time.Sleep(500 * time.Millisecond)

	if !rang(bell, time.Second) {
		t.Fatal("no ring")
	}
	if rang(bell, 200*time.Millisecond) {
		t.Fatal("doorbell should hold at most one pending ring")
	}
}

func TestTheHubReconnectsAndRingsEveryoneSoNothingIsMissed(t *testing.T) {
	db, url := testdb.NewWithURL(t)
	seedTrip(t, db, "t1")
	h := startHub(t, url)
	bell, cancel := h.Subscribe("t1")
	defer cancel()

	// Kill the hub's LISTEN connection.
	var killed int
	if err := db.QueryRow(`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE query = 'LISTEN trip_events' AND pid <> pg_backend_pid()`).Scan(&killed); err != nil || killed == 0 {
		t.Fatalf("could not find the listener connection (killed=%d, err=%v)", killed, err)
	}

	// Readers are told to re-query because notifications sent while it was down are lost.
	if !rang(bell, 10*time.Second) {
		t.Fatal("no ring after the listener reconnected")
	}
	// And the new connection delivers.
	appendCommitted(t, db, "t1", "images", map[string]any{})
	if !rang(bell, 5*time.Second) {
		t.Fatal("doorbell did not ring after reconnecting")
	}
}
