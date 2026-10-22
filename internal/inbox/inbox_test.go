package inbox_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/inbox"
	"github.com/sky-walkerx/trequila/internal/testdb"
)

func event(id string) bus.Event {
	e, _ := bus.NewEvent("trips.itinerary", id, map[string]string{})
	return e
}

func TestADuplicateDeliveryRunsTheHandlerOnce(t *testing.T) {
	db := testdb.New(t)
	var runs int
	h := inbox.Handle(db, "writer", func(ctx context.Context, tx *sql.Tx, e bus.Event) error {
		runs++
		return nil
	})

	for i := 0; i < 3; i++ {
		if err := h(context.Background(), event("evt-1")); err != nil {
			t.Fatal(err)
		}
	}

	if runs != 1 {
		t.Errorf("handler ran %d times, want 1", runs)
	}
}

func TestEachConsumerKeepsItsOwnLedger(t *testing.T) {
	db := testdb.New(t)
	var a, b int
	ha := inbox.Handle(db, "a", func(context.Context, *sql.Tx, bus.Event) error { a++; return nil })
	hb := inbox.Handle(db, "b", func(context.Context, *sql.Tx, bus.Event) error { b++; return nil })

	ha(context.Background(), event("evt-1"))
	hb(context.Background(), event("evt-1"))

	if a != 1 || b != 1 {
		t.Errorf("a=%d b=%d, want both to see the event once", a, b)
	}
}

func TestAFailingHandlerRollsBackItsWorkAndItsMarker(t *testing.T) {
	db := testdb.New(t)
	db.Exec(`CREATE TABLE effects (id TEXT)`)
	fail := true
	h := inbox.Handle(db, "writer", func(ctx context.Context, tx *sql.Tx, e bus.Event) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO effects VALUES ($1)`, e.ID); err != nil {
			return err
		}
		if fail {
			return errors.New("boom")
		}
		return nil
	})

	if err := h(context.Background(), event("evt-1")); err == nil {
		t.Fatal("expected the handler error to surface")
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM effects`).Scan(&n)
	if n != 0 {
		t.Fatalf("effects after failure = %d, want 0 (rolled back)", n)
	}

	fail = false
	if err := h(context.Background(), event("evt-1")); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT count(*) FROM effects`).Scan(&n)
	if n != 1 {
		t.Errorf("effects after retry = %d, want 1", n)
	}
}

func TestPermanentErrorsStillRollBackAndStayPermanent(t *testing.T) {
	db := testdb.New(t)
	h := inbox.Handle(db, "writer", func(context.Context, *sql.Tx, bus.Event) error {
		return bus.Permanent(errors.New("bad payload"))
	})
	err := h(context.Background(), event("evt-1"))
	if !bus.IsPermanent(err) {
		t.Fatalf("want a permanent error, got %v", err)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM processed_events`).Scan(&n)
	if n != 0 {
		t.Errorf("marker rows = %d, want 0", n)
	}
}

func TestConcurrentDuplicateDeliveriesApplyOnce(t *testing.T) {
	db := testdb.New(t)
	var runs atomic.Int32
	h := inbox.Handle(db, "writer", func(context.Context, *sql.Tx, bus.Event) error {
		runs.Add(1)
		time.Sleep(30 * time.Millisecond)
		return nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h(context.Background(), event("evt-1")) }()
	}
	wg.Wait()

	if runs.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", runs.Load())
	}
}

func TestPurgeDropsOldMarkers(t *testing.T) {
	db := testdb.New(t)
	h := inbox.Handle(db, "writer", func(context.Context, *sql.Tx, bus.Event) error { return nil })
	h(context.Background(), event("old"))
	h(context.Background(), event("fresh"))
	db.Exec(`UPDATE processed_events SET processed_at = now() - interval '10 days' WHERE event_id = 'old'`)

	n, err := inbox.Purge(context.Background(), db, 7*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v; want 1", n, err)
	}
}
