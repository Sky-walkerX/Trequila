package planner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/bus/memory"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/svc/planner"
	"github.com/sky-walkerx/trequila/internal/types"
)

type fakeKeys struct {
	cfg ai.ProviderConfig
	err error
}

func (f fakeKeys) Resolve(context.Context, string, string) (ai.ProviderConfig, error) {
	return f.cfg, f.err
}

// fakeProvider implements only what the planner calls.
type fakeProvider struct {
	ai.LLMProvider
	plan   []types.DayPlan
	err    error
	closed *bool
}

func (f fakeProvider) GenerateItinerary(context.Context, types.TripRequest) ([]types.DayPlan, error) {
	return f.plan, f.err
}
func (f fakeProvider) Close() error { *f.closed = true; return nil }

func requestEvent(t *testing.T) bus.Event {
	t.Helper()
	e, err := bus.NewEvent(events.TripRequested, events.ID(events.TripRequested, "t-1"), types.TripRequest{
		RequestID: "t-1", UserID: "u1", Source: "Berlin", Destination: "Lisbon",
		StartDate: "2026-12-01", EndDate: "2026-12-03", Days: 3, Provider: "gemini", Model: "m",
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func newPlanner(keys fakeKeys, p fakeProvider, pub bus.Publisher) *planner.Service {
	return planner.New(keys, pub, planner.WithProviderFactory(func(context.Context, ai.ProviderConfig) (ai.LLMProvider, error) {
		return p, nil
	}))
}

func handler(t *testing.T, s *planner.Service) bus.Handler {
	t.Helper()
	subs := s.Subscriptions()
	if len(subs) != 1 || subs[0].Type != events.TripRequested || subs[0].Durable == "" {
		t.Fatalf("subscriptions = %+v", subs)
	}
	return subs[0].Handler
}

func TestAnItineraryIsPublishedForARequest(t *testing.T) {
	b := memory.New()
	closed := false
	plan := []types.DayPlan{{DayIndex: 0, Date: "2026-12-01", Summary: "Alfama"}}
	s := newPlanner(fakeKeys{}, fakeProvider{plan: plan, closed: &closed}, b)

	if err := handler(t, s)(context.Background(), requestEvent(t)); err != nil {
		t.Fatal(err)
	}

	evs := b.Events()
	if len(evs) != 1 || evs[0].Type != events.TripItinerary || evs[0].ID != events.ID(events.TripItinerary, "t-1") {
		t.Fatalf("published = %+v", evs)
	}
	var got events.ItineraryGenerated
	if err := evs[0].Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "t-1" || got.Destination != "Lisbon" || len(got.Itinerary) != 1 || got.Itinerary[0].Summary != "Alfama" || got.GeneratedAt == "" {
		t.Errorf("payload = %+v", got)
	}
	if !closed {
		t.Error("the provider was not closed")
	}
}

func TestAnAuthFailureReportsTheTripFailedAndIsNotRetried(t *testing.T) {
	b := memory.New()
	closed := false
	s := newPlanner(fakeKeys{}, fakeProvider{err: errors.New("403 permission denied"), closed: &closed}, b)

	err := handler(t, s)(context.Background(), requestEvent(t))

	if !bus.IsPermanent(err) {
		t.Fatalf("want a permanent error, got %v", err)
	}
	evs := b.Events()
	if len(evs) != 1 || evs[0].Type != events.TripFailed {
		t.Fatalf("published = %+v", evs)
	}
	var f events.Failed
	evs[0].Decode(&f)
	if f.RequestID != "t-1" || f.Stage != "planner" || f.Reason != "API key invalid, expired, or out of credits." {
		t.Errorf("failure = %+v", f)
	}
	if !closed {
		t.Error("the provider was not closed")
	}
}

func TestATransientFailureIsRetriedAndPublishesNothing(t *testing.T) {
	b := memory.New()
	closed := false
	s := newPlanner(fakeKeys{}, fakeProvider{err: errors.New("503 upstream unavailable"), closed: &closed}, b)

	err := handler(t, s)(context.Background(), requestEvent(t))

	if err == nil || bus.IsPermanent(err) {
		t.Fatalf("want a retryable error, got %v", err)
	}
	if len(b.Events()) != 0 {
		t.Errorf("published %d events, want none", len(b.Events()))
	}
}

func TestAMissingKeyReportsTheTripFailed(t *testing.T) {
	b := memory.New()
	s := planner.New(fakeKeys{err: providerkeys.ErrNoKey}, b)

	err := handler(t, s)(context.Background(), requestEvent(t))

	if !bus.IsPermanent(err) {
		t.Fatalf("want a permanent error, got %v", err)
	}
	evs := b.Events()
	if len(evs) != 1 || evs[0].Type != events.TripFailed {
		t.Fatalf("published = %+v", evs)
	}
	var f events.Failed
	evs[0].Decode(&f)
	if f.Reason != "No configured API key found." {
		t.Errorf("reason = %q", f.Reason)
	}
}

func TestAProviderThatCannotBeBuiltReportsTheTripFailed(t *testing.T) {
	b := memory.New()
	s := planner.New(fakeKeys{}, b, planner.WithProviderFactory(func(context.Context, ai.ProviderConfig) (ai.LLMProvider, error) {
		return nil, errors.New("unsupported")
	}))

	err := handler(t, s)(context.Background(), requestEvent(t))

	if !bus.IsPermanent(err) {
		t.Fatalf("want a permanent error, got %v", err)
	}
	var f events.Failed
	b.Events()[0].Decode(&f)
	if f.Reason != "Invalid provider or unsupported model." {
		t.Errorf("reason = %q", f.Reason)
	}
}

func TestAnUndecodablePayloadIsPermanent(t *testing.T) {
	s := planner.New(fakeKeys{}, memory.New())
	err := handler(t, s)(context.Background(), bus.Event{ID: "x", Type: events.TripRequested, Data: []byte(`{"days":"many"}`)})
	if !bus.IsPermanent(err) {
		t.Fatalf("want a permanent error, got %v", err)
	}
}

func TestAKeyLookupOutageIsRetriedNotReportedAsAMissingKey(t *testing.T) {
	b := memory.New()
	s := planner.New(fakeKeys{err: errors.New("connection refused")}, b)

	err := handler(t, s)(context.Background(), requestEvent(t))

	if err == nil || bus.IsPermanent(err) {
		t.Fatalf("want a retryable error, got %v", err)
	}
	if len(b.Events()) != 0 {
		t.Errorf("published %d events, want none", len(b.Events()))
	}
}
