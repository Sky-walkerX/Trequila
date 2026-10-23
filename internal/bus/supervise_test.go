package bus_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-walkerx/trequila/internal/bus"
)

type flakyBus struct {
	bus.Bus
	failures int32
	calls    atomic.Int32
}

func (f *flakyBus) Subscribe(ctx context.Context, _ bus.Subscription) error {
	if f.calls.Add(1) <= f.failures {
		return errors.New("broker unreachable")
	}
	<-ctx.Done()
	return nil
}

func TestSuperviseRetriesASubscriptionThatFailedToStart(t *testing.T) {
	fb := &flakyBus{failures: 3}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bus.Supervise(ctx, fb, bus.Subscription{Durable: "d-v1"}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for fb.calls.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := fb.calls.Load(); got != 4 {
		t.Fatalf("Subscribe called %d times, want 4 (3 failures then a live subscription)", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return after the context was cancelled")
	}
}
