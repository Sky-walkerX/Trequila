package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/images"
	"github.com/sky-walkerx/trequila/internal/inbox"
	"github.com/sky-walkerx/trequila/internal/metrics"
	"github.com/sky-walkerx/trequila/internal/outbox"
	"github.com/sky-walkerx/trequila/internal/tripevents"
	"github.com/sky-walkerx/trequila/internal/types"
)

// Stream event types, as clients see them in the "event:" field.
const (
	streamImages    = "images"
	streamPlaces    = "places"
	streamItinerary = "itinerary"
	streamRoute     = "route"
	streamFailed    = "failed"
)

// Subscriptions lists the result events the API persists. Each handler runs inside an
// inbox transaction: the trips row, the trip event the stream replays and any
// follow-up outbox event commit together, and a redelivery changes nothing.
func (s *Service) Subscriptions() []bus.Subscription {
	sub := func(durable, typ string, fn func(context.Context, *sql.Tx, bus.Event) error) bus.Subscription {
		h := inbox.Handle(s.db, durable, fn)
		return bus.Subscription{
			Durable:    durable,
			Type:       typ,
			AckWait:    30 * time.Second,
			MaxDeliver: 10, // with the doubling delay, about five minutes to ride out a database outage
			Handler: func(ctx context.Context, e bus.Event) error {
				if err := h(ctx, e); err != nil {
					return err
				}
				if s.relay != nil {
					s.relay.Kick(ctx) // publish any follow-up event now rather than at the next sweep
				}
				return nil
			},
		}
	}
	return []bus.Subscription{
		sub("api-itinerary-v4", events.TripItinerary, s.applyItinerary),
		sub("api-images-v4", events.TripImages, s.applyImages),
		sub("api-places-v4", events.TripPlaces, s.applyPlaces),
		sub("api-route-v4", events.TripRoute, s.applyRoute),
		sub("api-failures-v4", events.TripFailed, s.applyFailure),
	}
}

// frame builds the JSON object a stream event carries: {"type": <t>, <key>: <value>}.
func frame(typ, key string, value any) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"type": typ, key: value})
}

// record updates one trips column and appends the matching trip event. It reports
// false when the trip no longer exists (deleted while the event was in flight).
func record(ctx context.Context, tx *sql.Tx, tripID, update string, value any, evType string, ev json.RawMessage) (bool, error) {
	res, err := tx.ExecContext(ctx, update, value, tripID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tripevents.Append(ctx, tx, tripID, evType, ev); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) applyItinerary(ctx context.Context, tx *sql.Tx, e bus.Event) error {
	var p events.ItineraryGenerated
	if err := e.Decode(&p); err != nil {
		return err
	}
	itinerary, err := json.Marshal(p.Itinerary)
	if err != nil {
		return bus.Permanent(err)
	}
	ev, err := frame(streamItinerary, "itinerary", p.Itinerary)
	if err != nil {
		return bus.Permanent(err)
	}
	ok, err := record(ctx, tx, p.RequestID,
		`UPDATE trips SET status = 'completed', error_reason = NULL, itinerary = $1, updated_at = NOW() WHERE request_id = $2`,
		string(itinerary), streamItinerary, ev)
	if err != nil || !ok {
		return err
	}
	metrics.TripsCompletedTotal.Inc()

	// The router has no database, so the request carries the places the trip already
	// has. Places may still be in flight; the router falls back to geocoding.
	return s.addRouteRequest(ctx, tx, p.RequestID, p.Destination, p.Itinerary, "")
}

func (s *Service) applyImages(ctx context.Context, tx *sql.Tx, e bus.Event) error {
	var p events.ImagesFound
	if err := e.Decode(&p); err != nil {
		return err
	}
	if p.Images == nil {
		p.Images = []images.Image{}
	}
	raw, _ := json.Marshal(p.Images)
	ev, err := frame(streamImages, "images", p.Images)
	if err != nil {
		return bus.Permanent(err)
	}
	_, err = record(ctx, tx, p.RequestID, `UPDATE trips SET images = $1 WHERE request_id = $2`, string(raw), streamImages, ev)
	return err
}

func (s *Service) applyPlaces(ctx context.Context, tx *sql.Tx, e bus.Event) error {
	var p events.PlacesFound
	if err := e.Decode(&p); err != nil {
		return err
	}
	raw, _ := json.Marshal(p.Places)
	ev, err := frame(streamPlaces, "places", p.Places)
	if err != nil {
		return bus.Permanent(err)
	}
	_, err = record(ctx, tx, p.RequestID, `UPDATE trips SET places = $1 WHERE request_id = $2`, string(raw), streamPlaces, ev)
	return err
}

// applyRoute stores the route. A trip is already 'completed' by the time its route
// exists, so only the route column and its event change.
func (s *Service) applyRoute(ctx context.Context, tx *sql.Tx, e bus.Event) error {
	var p events.RouteComputed
	if err := e.Decode(&p); err != nil {
		return err
	}
	raw, _ := json.Marshal(p.Route)
	ev, err := frame(streamRoute, "route", p.Route)
	if err != nil {
		return bus.Permanent(err)
	}
	_, err = record(ctx, tx, p.RequestID, `UPDATE trips SET route = $1 WHERE request_id = $2`, string(raw), streamRoute, ev)
	return err
}

func (s *Service) applyFailure(ctx context.Context, tx *sql.Tx, e bus.Event) error {
	var p events.Failed
	if err := e.Decode(&p); err != nil {
		return err
	}
	return s.markFailed(ctx, tx, p.RequestID, p.Reason)
}

// markFailed fails a trip that is still pending and appends the event the stream ends
// on. A trip that already has an itinerary is left alone.
func (s *Service) markFailed(ctx context.Context, tx *sql.Tx, tripID, reason string) error {
	ev, err := frame(streamFailed, "reason", reason)
	if err != nil {
		return bus.Permanent(err)
	}
	ok, err := record(ctx, tx, tripID,
		`UPDATE trips SET status = 'failed', error_reason = $1, updated_at = NOW() WHERE request_id = $2 AND status = 'pending'`,
		reason, streamFailed, ev)
	if err != nil {
		return err
	}
	if ok {
		metrics.TripsFailedTotal.Inc()
	}
	return nil
}

// addRouteRequest queues a route computation for the trip. idSuffix distinguishes
// repeated requests (the optimise endpoint) from the first one.
func (s *Service) addRouteRequest(ctx context.Context, tx *sql.Tx, tripID, destination string, itinerary []types.DayPlan, idSuffix string) error {
	if len(itinerary) == 0 {
		return nil
	}
	var placesJSON []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(places, '{}'::jsonb) FROM trips WHERE request_id = $1`, tripID).Scan(&placesJSON); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read places: %w", err)
	}
	id := events.ID(events.TripRouteRequested, tripID)
	if idSuffix != "" {
		id += ":" + idSuffix
	}
	ev, err := bus.NewEvent(events.TripRouteRequested, id, events.RouteRequested{
		RequestID:   tripID,
		Destination: destination,
		Itinerary:   itinerary,
		Places:      flattenPlaces(placesJSON),
	})
	if err != nil {
		return err
	}
	return outbox.Add(ctx, tx, ev)
}
