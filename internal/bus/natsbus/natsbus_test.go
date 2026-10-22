package natsbus_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/bus/natsbus"
)

func startServer(t *testing.T) string {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server did not start")
	}
	t.Cleanup(func() { ns.Shutdown(); ns.WaitForShutdown() })
	return ns.ClientURL()
}

func connect(t *testing.T) *natsbus.Bus {
	t.Helper()
	b, err := natsbus.Connect(context.Background(), natsbus.Config{URL: startServer(t), RetryDelay: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

func subscribe(t *testing.T, b *natsbus.Bus, s bus.Subscription) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.Subscribe(ctx, s) }()
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustEvent(t *testing.T, typ, id string) bus.Event {
	t.Helper()
	e, err := bus.NewEvent(typ, id, map[string]string{"hello": "world"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPublishedEventsReachTheHandlerIntact(t *testing.T) {
	b := connect(t)
	got := make(chan bus.Event, 1)
	subscribe(t, b, bus.Subscription{Durable: "t-v1", Type: "trips.requested", Handler: func(_ context.Context, e bus.Event) error {
		got <- e
		return nil
	}})

	sent := mustEvent(t, "trips.requested", "evt-1")
	sent.TraceParent = "00-abc-def-01"
	if err := b.Publish(context.Background(), sent); err != nil {
		t.Fatal(err)
	}

	select {
	case e := <-got:
		if e.ID != "evt-1" || e.Type != "trips.requested" || e.TraceParent != "00-abc-def-01" || string(e.Data) != `{"hello":"world"}` {
			t.Errorf("event changed in transit: %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery")
	}
}

func TestTransientErrorsAreRedelivered(t *testing.T) {
	b := connect(t)
	var attempts atomic.Int32
	subscribe(t, b, bus.Subscription{Durable: "t-v1", Type: "trips.requested", MaxDeliver: 5, Handler: func(context.Context, bus.Event) error {
		if attempts.Add(1) < 3 {
			return errors.New("flaky")
		}
		return nil
	}})

	b.Publish(context.Background(), mustEvent(t, "trips.requested", "evt-1"))

	eventually(t, "the third attempt", func() bool { return attempts.Load() == 3 })
	time.Sleep(150 * time.Millisecond)
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3", attempts.Load())
	}
}

func TestPermanentErrorsAreDeliveredOnce(t *testing.T) {
	b := connect(t)
	var attempts atomic.Int32
	subscribe(t, b, bus.Subscription{Durable: "t-v1", Type: "trips.requested", MaxDeliver: 5, Handler: func(context.Context, bus.Event) error {
		attempts.Add(1)
		return bus.Permanent(errors.New("never"))
	}})

	b.Publish(context.Background(), mustEvent(t, "trips.requested", "evt-1"))

	eventually(t, "the first attempt", func() bool { return attempts.Load() == 1 })
	time.Sleep(300 * time.Millisecond)
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want 1", attempts.Load())
	}
}

func TestEventsStopAfterMaxDeliver(t *testing.T) {
	b := connect(t)
	var attempts atomic.Int32
	subscribe(t, b, bus.Subscription{Durable: "t-v1", Type: "trips.requested", MaxDeliver: 2, Handler: func(context.Context, bus.Event) error {
		attempts.Add(1)
		return errors.New("always")
	}})

	b.Publish(context.Background(), mustEvent(t, "trips.requested", "evt-1"))

	eventually(t, "two attempts", func() bool { return attempts.Load() == 2 })
	time.Sleep(400 * time.Millisecond)
	if attempts.Load() != 2 {
		t.Errorf("attempts = %d, want exactly MaxDeliver (2)", attempts.Load())
	}
}

func TestPublishingTheSameIDTwiceDeliversOnce(t *testing.T) {
	b := connect(t)
	var got atomic.Int32
	subscribe(t, b, bus.Subscription{Durable: "t-v1", Type: "trips.requested", Handler: func(context.Context, bus.Event) error {
		got.Add(1)
		return nil
	}})

	b.Publish(context.Background(), mustEvent(t, "trips.requested", "same"))
	b.Publish(context.Background(), mustEvent(t, "trips.requested", "same"))

	eventually(t, "first delivery", func() bool { return got.Load() >= 1 })
	time.Sleep(200 * time.Millisecond)
	if got.Load() != 1 {
		t.Errorf("deliveries = %d, want 1", got.Load())
	}
}

func TestLegacyEnvelopesWithoutAnIDAreSkipped(t *testing.T) {
	url := startServer(t)
	b, err := natsbus.Connect(context.Background(), natsbus.Config{URL: url, RetryDelay: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	var ids atomic.Value
	ids.Store([]string{})
	var calls atomic.Int32
	subscribe(t, b, bus.Subscription{Durable: "t-v1", Type: "trips.requested", Handler: func(_ context.Context, e bus.Event) error {
		calls.Add(1)
		ids.Store(append(ids.Load().([]string), e.ID))
		return nil
	}})

	// The pre-bus envelope: {"type": "...", "data": {...}} with no id.
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if err := nc.Publish("trips.requested", []byte(`{"type":"TripRequested","data":{"request_id":"old"}}`)); err != nil {
		t.Fatal(err)
	}
	nc.Flush()
	b.Publish(context.Background(), mustEvent(t, "trips.requested", "new-1"))

	eventually(t, "the new event", func() bool { return calls.Load() >= 1 })
	time.Sleep(100 * time.Millisecond)
	got := ids.Load().([]string)
	if len(got) != 1 || got[0] != "new-1" {
		t.Errorf("handler saw %v, want only [new-1]", got)
	}
}

func TestPing(t *testing.T) {
	b := connect(t)
	if err := b.Ping(context.Background()); err != nil {
		t.Errorf("ping: %v", err)
	}
	b.Close()
	if err := b.Ping(context.Background()); err == nil {
		t.Error("ping after close should fail")
	}
}
