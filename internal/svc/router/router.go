// Package router orders each day of an itinerary into a visiting route. It consumes
// trips.route_requested and publishes trips.route.
//
// The router has no database. The request carries the itinerary and the places the
// trip already found; anything still unplaced is geocoded, rate-limited, within a time
// budget so one job cannot outlive its acknowledgement window.
package router

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/route"
)

// geocodeBudget bounds one job's wall-clock time. Lookups are rate-limited to about
// one a second, so a long trip full of unfamiliar names would otherwise keep the
// message in flight past its ack wait and be redelivered mid-flight.
const geocodeBudget = 3 * time.Minute

// ackWait must exceed geocodeBudget plus the lookup that may be running when the
// budget expires.
const ackWait = 5 * time.Minute

var _ route.Geocoder = (*geo.Service)(nil)

// Service implements the router.
type Service struct {
	geocoder route.Geocoder
	pub      bus.Publisher
	log      *slog.Logger
}

// New creates the router. geocoder may be nil, in which case only the trip's own
// places can place a stop.
func New(geocoder route.Geocoder, pub bus.Publisher) *Service {
	return &Service{geocoder: geocoder, pub: pub, log: slog.Default()}
}

// WithLogger sets the logger.
func (s *Service) WithLogger(l *slog.Logger) *Service { s.log = l; return s }

// Subscriptions lists what the router consumes.
func (s *Service) Subscriptions() []bus.Subscription {
	return []bus.Subscription{{
		Durable:    "route-v4",
		Type:       events.TripRouteRequested,
		AckWait:    ackWait,
		MaxDeliver: 3,
		Handler:    s.handle,
	}}
}

func (s *Service) handle(ctx context.Context, e bus.Event) error {
	var req events.RouteRequested
	if err := e.Decode(&req); err != nil {
		return err
	}
	if req.RequestID == "" || len(req.Itinerary) == 0 {
		return bus.Permanent(fmt.Errorf("route request %s has nothing to route", e.ID))
	}

	locator := route.NewPlaceLocator(req.Destination, req.Places, s.geocoder).
		WithDeadline(time.Now().Add(geocodeBudget))
	computed := route.Build(req.Itinerary, locator)

	// The result ID derives from the request's ID: redelivering one request
	// de-duplicates, while a later re-optimise request yields a new result.
	out, err := bus.NewEvent(events.TripRoute, events.TripRoute+":"+e.ID, events.RouteComputed{
		RequestID: req.RequestID, Route: computed,
	})
	if err != nil {
		return err
	}
	if err := s.pub.Publish(ctx, out); err != nil {
		return fmt.Errorf("publish route: %w", err)
	}
	s.log.Info("route computed", "trip_id", req.RequestID, "days", len(computed.Days))
	return nil
}
