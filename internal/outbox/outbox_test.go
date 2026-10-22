package outbox_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/outbox"
	"github.com/sky-walkerx/trequila/internal/testdb"
)

type recorder struct {
	mu   sync.Mutex
	ids  map[string]int
	fail func(bus.Event) error
}

func newRecorder() *recorder { return &recorder{ids: map[string]int{}} }

func (r *recorder) Publish(_ context.Context, e bus.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		if err := r.fail(e); err != nil {
			return err
		}
	}
	r.ids[e.ID]++
	return nil
}

func (r *recorder) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ids[id]
}

func add(t *testing.T, db *sql.DB, ids ...string) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		e, _ := bus.NewEvent("trips.requested", id, map[string]string{"id": id})
		if err := outbox.Add(context.Background(), tx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRolledBackEventsAreNeverPublished(t *testing.T) {
	db := testdb.New(t)
	rec := newRecorder()
	relay := &outbox.Relay{DB: db, Pub: rec}

	tx, _ := db.Begin()
	e, _ := bus.NewEvent("trips.requested", "gone", map[string]string{})
	if err := outbox.Add(context.Background(), tx, e); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()

	n, err := relay.Flush(context.Background(), 10)
	if err != nil || n != 0 || rec.count("gone") != 0 {
		t.Fatalf("flush = %d, %v, published %d", n, err, rec.count("gone"))
	}
}

func TestCommittedEventsPublishOnceAndAreMarked(t *testing.T) {
	db := testdb.New(t)
	rec := newRecorder()
	relay := &outbox.Relay{DB: db, Pub: rec}
	add(t, db, "e1", "e2")

	n, err := relay.Flush(context.Background(), 10)
	if err != nil || n != 2 {
		t.Fatalf("flush = %d, %v", n, err)
	}
	n, err = relay.Flush(context.Background(), 10)
	if err != nil || n != 0 {
		t.Fatalf("second flush = %d, %v; want nothing left", n, err)
	}
	if rec.count("e1") != 1 || rec.count("e2") != 1 {
		t.Errorf("published e1=%d e2=%d, want 1 each", rec.count("e1"), rec.count("e2"))
	}
}

func TestAddingTheSameEventIDTwiceKeepsOneRow(t *testing.T) {
	db := testdb.New(t)
	add(t, db, "dup")
	add(t, db, "dup")

	var n int
	db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&n)
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestAFailingPublishLeavesTheRowPendingAndRecordsWhy(t *testing.T) {
	db := testdb.New(t)
	rec := newRecorder()
	rec.fail = func(bus.Event) error { return errors.New("broker down") }
	relay := &outbox.Relay{DB: db, Pub: rec}
	add(t, db, "e1")

	n, err := relay.Flush(context.Background(), 10)
	if err == nil || n != 0 {
		t.Fatalf("flush = %d, %v; want a failure", n, err)
	}
	var attempts int
	var lastErr sql.NullString
	var published sql.NullTime
	db.QueryRow(`SELECT attempts, last_error, published_at FROM outbox WHERE event_id='e1'`).Scan(&attempts, &lastErr, &published)
	if attempts != 1 || lastErr.String != "broker down" || published.Valid {
		t.Errorf("attempts=%d last_error=%q published=%v", attempts, lastErr.String, published.Valid)
	}

	rec.fail = nil
	n, err = relay.Flush(context.Background(), 10)
	if err != nil || n != 1 || rec.count("e1") != 1 {
		t.Fatalf("recovery flush = %d, %v, published %d", n, err, rec.count("e1"))
	}
}

func TestConcurrentRelaysNeverPublishAnEventTwice(t *testing.T) {
	db := testdb.New(t)
	rec := newRecorder()
	var ids []string
	for i := 0; i < 40; i++ {
		ids = append(ids, fmt.Sprintf("e%02d", i))
	}
	add(t, db, ids...)

	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			relay := &outbox.Relay{DB: db, Pub: rec}
			for {
				n, err := relay.Flush(context.Background(), 5)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	for _, id := range ids {
		if c := rec.count(id); c != 1 {
			t.Errorf("%s published %d times, want 1", id, c)
		}
	}
}

func TestKickPublishesWithoutReturningErrors(t *testing.T) {
	db := testdb.New(t)
	rec := newRecorder()
	relay := &outbox.Relay{DB: db, Pub: rec}
	add(t, db, "e1")

	relay.Kick(context.Background())

	if rec.count("e1") != 1 {
		t.Errorf("e1 published %d times, want 1", rec.count("e1"))
	}
}

func TestPurgeRemovesOnlyOldPublishedRows(t *testing.T) {
	db := testdb.New(t)
	rec := newRecorder()
	relay := &outbox.Relay{DB: db, Pub: rec}
	add(t, db, "old", "fresh", "pending")
	relay.Flush(context.Background(), 2) // publishes old and fresh
	db.Exec(`UPDATE outbox SET published_at = now() - interval '10 days' WHERE event_id = 'old'`)

	n, err := relay.Purge(context.Background(), 7*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v; want 1", n, err)
	}
	var left int
	db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&left)
	if left != 2 {
		t.Errorf("rows left = %d, want 2 (fresh published and pending)", left)
	}
}
