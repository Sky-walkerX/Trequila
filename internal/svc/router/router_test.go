package router_test

import (
	"context"
	"testing"

	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/bus/memory"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/svc/router"
	"github.com/sky-walkerx/trequila/internal/types"
)

// noGeocoder fails every lookup, so only the trip's own places can place a stop.
type noGeocoder struct{ calls int }

func (g *noGeocoder) GetCoordinates(string) (float64, float64, error) {
	g.calls++
	return 0, 0, context.DeadlineExceeded
}

func handler(t *testing.T, s *router.Service) bus.Handler {
	t.Helper()
	subs := s.Subscriptions()
	if len(subs) != 1 || subs[0].Type != events.TripRouteRequested || subs[0].Durable != "route-v4" {
		t.Fatalf("subscriptions = %+v", subs)
	}
	return subs[0].Handler
}

func request(t *testing.T, id string, data events.RouteRequested) bus.Event {
	t.Helper()
	e, err := bus.NewEvent(events.TripRouteRequested, id, data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestARouteIsComputedFromTheTripsOwnPlaces(t *testing.T) {
	b := memory.New()
	g := &noGeocoder{}
	s := router.New(g, b)
	req := request(t, "trips.route_requested:t-1", events.RouteRequested{
		RequestID:   "t-1",
		Destination: "Lisbon",
		Itinerary: []types.DayPlan{{DayIndex: 0, Activities: []types.Activity{
			{Name: "Belem Tower", Time: "10:00"}, {Name: "Alfama", Time: "14:00"},
		}}},
		Places: []geo.Place{
			{Name: "Belem Tower", Lat: 38.6916, Lon: -9.2160},
			{Name: "Alfama", Lat: 38.7110, Lon: -9.1300},
		},
	})

	if err := handler(t, s)(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	evs := b.Events()
	if len(evs) != 1 || evs[0].Type != events.TripRoute || evs[0].ID != "trips.route:trips.route_requested:t-1" {
		t.Fatalf("published = %+v", evs)
	}
	var got events.RouteComputed
	if err := evs[0].Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "t-1" || len(got.Route.Days) != 1 || len(got.Route.Days[0].Ordered) != 2 {
		t.Errorf("route = %+v", got.Route)
	}
	if g.calls != 0 {
		t.Errorf("geocoder called %d times; the trip's places should have been enough", g.calls)
	}
}

func TestEachRouteRequestProducesItsOwnResult(t *testing.T) {
	b := memory.New()
	s := router.New(&noGeocoder{}, b)
	data := events.RouteRequested{RequestID: "t-1", Itinerary: []types.DayPlan{{Activities: []types.Activity{{Name: "A"}}}}}

	handler(t, s)(context.Background(), request(t, "trips.route_requested:t-1", data))
	handler(t, s)(context.Background(), request(t, "trips.route_requested:t-1:optimize-2", data))

	evs := b.Events()
	if len(evs) != 2 || evs[0].ID == evs[1].ID {
		t.Fatalf("want two distinct result events, got %+v", evs)
	}
}

func TestARequestWithNothingToRouteIsDroppedNotRetried(t *testing.T) {
	b := memory.New()
	s := router.New(&noGeocoder{}, b)

	for name, data := range map[string]events.RouteRequested{
		"no trip id":   {Itinerary: []types.DayPlan{{}}},
		"no itinerary": {RequestID: "t-1"},
	} {
		err := handler(t, s)(context.Background(), request(t, "e-"+name, data))
		if !bus.IsPermanent(err) {
			t.Errorf("%s: want a permanent error, got %v", name, err)
		}
	}
	if len(b.Events()) != 0 {
		t.Errorf("published %d events, want none", len(b.Events()))
	}
}
