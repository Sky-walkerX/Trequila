package memory_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/bus/memory"
)

func run(t *testing.T, b *memory.Bus, s bus.Subscription) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.Subscribe(ctx, s) }()
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func ev(t *testing.T, typ, id string) bus.Event {
	t.Helper()
	e, err := bus.NewEvent(typ, id, map[string]string{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDeliversEventsOfTheSubscribedTypeOnly(t *testing.T) {
	b := memory.New()
	var got atomic.Int32
	run(t, b, bus.Subscription{Durable: "d", Type: "a", Handler: func(context.Context, bus.Event) error {
		got.Add(1)
		return nil
	}})

	b.Publish(context.Background(), ev(t, "a", "1"))
	b.Publish(context.Background(), ev(t, "b", "2"))
	b.Publish(context.Background(), ev(t, "a", "3"))

	eventually(t, "two deliveries", func() bool { return got.Load() == 2 })
	time.Sleep(30 * time.Millisecond)
	if got.Load() != 2 {
		t.Errorf("deliveries = %d, want 2", got.Load())
	}
}

func TestEventsPublishedBeforeTheSubscriberStartsAreStillDelivered(t *testing.T) {
	b := memory.New()
	b.Publish(context.Background(), ev(t, "a", "1"))
	var got atomic.Int32
	run(t, b, bus.Subscription{Durable: "d", Type: "a", Handler: func(context.Context, bus.Event) error {
		got.Add(1)
		return nil
	}})
	eventually(t, "the backlog delivery", func() bool { return got.Load() == 1 })
}

func TestTransientErrorsAreRedeliveredUpToMaxDeliver(t *testing.T) {
	b := memory.New()
	var attempts atomic.Int32
	run(t, b, bus.Subscription{Durable: "d", Type: "a", MaxDeliver: 3, Handler: func(context.Context, bus.Event) error {
		attempts.Add(1)
		return errors.New("try again")
	}})

	b.Publish(context.Background(), ev(t, "a", "1"))

	eventually(t, "three attempts", func() bool { return attempts.Load() == 3 })
	time.Sleep(30 * time.Millisecond)
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want exactly 3", attempts.Load())
	}
}

func TestARetryThatSucceedsStopsRedelivery(t *testing.T) {
	b := memory.New()
	var attempts atomic.Int32
	run(t, b, bus.Subscription{Durable: "d", Type: "a", MaxDeliver: 5, Handler: func(context.Context, bus.Event) error {
		if attempts.Add(1) < 2 {
			return errors.New("flaky")
		}
		return nil
	}})

	b.Publish(context.Background(), ev(t, "a", "1"))

	eventually(t, "second attempt", func() bool { return attempts.Load() == 2 })
	time.Sleep(30 * time.Millisecond)
	if attempts.Load() != 2 {
		t.Errorf("attempts = %d, want 2", attempts.Load())
	}
}

func TestPermanentErrorsAreNotRedelivered(t *testing.T) {
	b := memory.New()
	var attempts atomic.Int32
	run(t, b, bus.Subscription{Durable: "d", Type: "a", MaxDeliver: 5, Handler: func(context.Context, bus.Event) error {
		attempts.Add(1)
		return bus.Permanent(errors.New("never going to work"))
	}})

	b.Publish(context.Background(), ev(t, "a", "1"))

	eventually(t, "one attempt", func() bool { return attempts.Load() == 1 })
	time.Sleep(30 * time.Millisecond)
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want 1", attempts.Load())
	}
}

func TestPublishingTheSameIDTwiceDeliversOnce(t *testing.T) {
	b := memory.New()
	var got atomic.Int32
	run(t, b, bus.Subscription{Durable: "d", Type: "a", Handler: func(context.Context, bus.Event) error {
		got.Add(1)
		return nil
	}})

	b.Publish(context.Background(), ev(t, "a", "same"))
	b.Publish(context.Background(), ev(t, "a", "same"))

	eventually(t, "first delivery", func() bool { return got.Load() >= 1 })
	time.Sleep(30 * time.Millisecond)
	if got.Load() != 1 {
		t.Errorf("deliveries = %d, want 1 (publish dedupes on event ID, like JetStream)", got.Load())
	}
}

func TestEachDurableGetsItsOwnCopy(t *testing.T) {
	b := memory.New()
	var a, c atomic.Int32
	run(t, b, bus.Subscription{Durable: "one", Type: "x", Handler: func(context.Context, bus.Event) error { a.Add(1); return nil }})
	run(t, b, bus.Subscription{Durable: "two", Type: "x", Handler: func(context.Context, bus.Event) error { c.Add(1); return nil }})

	b.Publish(context.Background(), ev(t, "x", "1"))

	eventually(t, "both durables", func() bool { return a.Load() == 1 && c.Load() == 1 })
}
