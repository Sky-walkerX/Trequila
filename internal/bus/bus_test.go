package bus_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sky-walkerx/trequila/internal/bus"
)

type payload struct {
	TripID string `json:"trip_id"`
	N      int    `json:"n"`
}

func TestEventRoundTripsItsPayload(t *testing.T) {
	ev, err := bus.NewEvent("trips.requested", "evt-1", payload{TripID: "t-1", N: 3})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "evt-1" || ev.Type != "trips.requested" {
		t.Fatalf("unexpected envelope: %+v", ev)
	}
	var got payload
	if err := ev.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got != (payload{TripID: "t-1", N: 3}) {
		t.Errorf("payload = %+v", got)
	}
}

func TestNewEventGeneratesAnIDWhenNoneIsGiven(t *testing.T) {
	ev, err := bus.NewEvent("trips.requested", "", payload{})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID == "" {
		t.Error("expected a generated ID")
	}
}

func TestDecodeFailureIsPermanent(t *testing.T) {
	ev := bus.Event{ID: "x", Type: "t", Data: []byte(`{"n":"not a number"}`)}
	err := ev.Decode(&payload{})
	if err == nil || !bus.IsPermanent(err) {
		t.Fatalf("a payload that cannot decode will never decode; want a permanent error, got %v", err)
	}
}

func TestPermanentWrapsAndUnwraps(t *testing.T) {
	root := errors.New("bad key")
	err := bus.Permanent(root)
	if !bus.IsPermanent(err) {
		t.Error("IsPermanent = false")
	}
	if !errors.Is(err, root) {
		t.Error("errors.Is should see the wrapped error")
	}
	if bus.IsPermanent(root) {
		t.Error("a plain error is not permanent")
	}
	if bus.Permanent(nil) != nil {
		t.Error("Permanent(nil) should be nil")
	}
}

func TestRecoverTurnsAPanicIntoARetryableError(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	h := bus.Chain(func(context.Context, bus.Event) error { panic("boom") }, bus.Recover(log))

	err := h(context.Background(), bus.Event{ID: "e1", Type: "t"})

	if err == nil || bus.IsPermanent(err) {
		t.Fatalf("want a retryable error, got %v", err)
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("panic not logged: %s", buf.String())
	}
}

func TestChainRunsMiddlewareOutermostFirst(t *testing.T) {
	var order []string
	mk := func(name string) bus.Middleware {
		return func(next bus.Handler) bus.Handler {
			return func(ctx context.Context, e bus.Event) error {
				order = append(order, name+":in")
				err := next(ctx, e)
				order = append(order, name+":out")
				return err
			}
		}
	}
	h := bus.Chain(func(context.Context, bus.Event) error {
		order = append(order, "handler")
		return nil
	}, mk("a"), mk("b"))

	if err := h(context.Background(), bus.Event{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "a:in,b:in,handler,b:out,a:out" {
		t.Errorf("order = %s", got)
	}
}

func TestLoggingRecordsFailuresAtWarn(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	h := bus.Chain(func(context.Context, bus.Event) error { return errors.New("upstream down") }, bus.Logging(log, "planner"))

	_ = h(context.Background(), bus.Event{ID: "e1", Type: "trips.requested"})

	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "upstream down") || !strings.Contains(out, "planner") {
		t.Errorf("unexpected log: %s", out)
	}
}
