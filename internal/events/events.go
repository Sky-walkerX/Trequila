// Package events names the events services exchange and defines their payloads.
//
// An event's Type is its routing key. IDs are deterministic (see ID) wherever one
// input should produce one output, so a redelivered input re-publishes an event that
// the broker and the consumers recognise as the one they already saw.
package events

import (
	"encoding/json"

	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/images"
	"github.com/sky-walkerx/trequila/internal/route"
	"github.com/sky-walkerx/trequila/internal/types"
)

// Event types.
const (
	TripRequested      = "trips.requested"       // payload: types.TripRequest
	TripItinerary      = "trips.itinerary"       // payload: ItineraryGenerated
	TripImages         = "trips.images"          // payload: ImagesFound
	TripPlaces         = "trips.places"          // payload: PlacesFound
	TripRouteRequested = "trips.route_requested" // payload: RouteRequested
	TripRoute          = "trips.route"           // payload: RouteComputed
	TripFailed         = "trips.failed"          // payload: Failed
)

// ID returns the deterministic event ID for typ and a trip.
func ID(typ, requestID string) string { return typ + ":" + requestID }

// ItineraryGenerated is published by the planner.
type ItineraryGenerated struct {
	RequestID   string          `json:"request_id"`
	UserID      string          `json:"user_id"`
	Source      string          `json:"source"`
	Destination string          `json:"destination"`
	StartDate   string          `json:"start_date"`
	EndDate     string          `json:"end_date"`
	Itinerary   []types.DayPlan `json:"itinerary"`
	GeneratedAt string          `json:"generated_at"`
}

// ImagesFound is published by discovery.
type ImagesFound struct {
	RequestID string         `json:"request_id"`
	Images    []images.Image `json:"images"`
}

// PlaceGroups are the nearby places by kind. Empty groups marshal as [] so a missing
// kind is never a JSON null.
type PlaceGroups struct {
	Hotels      []geo.Place `json:"hotels"`
	Restaurants []geo.Place `json:"restaurants"`
	Attractions []geo.Place `json:"attractions"`
}

// MarshalJSON writes nil groups as empty arrays.
func (g PlaceGroups) MarshalJSON() ([]byte, error) {
	type alias PlaceGroups
	if g.Hotels == nil {
		g.Hotels = []geo.Place{}
	}
	if g.Restaurants == nil {
		g.Restaurants = []geo.Place{}
	}
	if g.Attractions == nil {
		g.Attractions = []geo.Place{}
	}
	return json.Marshal(alias(g))
}

// All returns every place in one slice.
func (g PlaceGroups) All() []geo.Place {
	out := make([]geo.Place, 0, len(g.Hotels)+len(g.Restaurants)+len(g.Attractions))
	out = append(out, g.Hotels...)
	out = append(out, g.Restaurants...)
	return append(out, g.Attractions...)
}

// PlacesFound is published by discovery.
type PlacesFound struct {
	RequestID string      `json:"request_id"`
	Places    PlaceGroups `json:"places"`
}

// RouteRequested asks the router to order a trip's days. It carries the places the
// trip already has so the router geocodes as little as possible; the router has no
// database of its own.
type RouteRequested struct {
	RequestID   string          `json:"request_id"`
	Destination string          `json:"destination"`
	Itinerary   []types.DayPlan `json:"itinerary"`
	Places      []geo.Place     `json:"places"`
}

// RouteComputed is published by the router.
type RouteComputed struct {
	RequestID string     `json:"request_id"`
	Route     route.Data `json:"route"`
}

// Failed reports that a stage could not produce its result.
type Failed struct {
	RequestID string `json:"request_id"`
	Stage     string `json:"stage"`
	Reason    string `json:"reason"`
}
