// Package discovery finds what exists at a trip's destination: nearby places from
// OpenStreetMap and destination images from Wikipedia and Openverse. It consumes
// trips.requested and publishes trips.places and trips.images.
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/images"
	"github.com/sky-walkerx/trequila/internal/types"
)

// Geo is the part of geo.Service discovery uses.
type Geo interface {
	GetCoordinates(city string) (lat, lon float64, err error)
	SearchPlaces(lat, lon float64, categories string, limit int) ([]geo.Place, error)
}

// Images is the part of images.Service discovery uses.
type Images interface {
	GetImages(ctx context.Context, query string) ([]images.Image, error)
}

var (
	_ Geo    = (*geo.Service)(nil)
	_ Images = (*images.Service)(nil)
)

// Service implements discovery.
type Service struct {
	geo    Geo
	images Images
	pub    bus.Publisher
	log    *slog.Logger
}

// New creates the service.
func New(g Geo, img Images, pub bus.Publisher) *Service {
	return &Service{geo: g, images: img, pub: pub, log: slog.Default()}
}

// WithLogger sets the logger.
func (s *Service) WithLogger(l *slog.Logger) *Service { s.log = l; return s }

// Subscriptions lists what discovery consumes. Places needs a long ack wait: three
// sequential Overpass queries against a dense city routinely take over a minute, and
// a shorter wait would redeliver the request while it is still being worked on.
func (s *Service) Subscriptions() []bus.Subscription {
	return []bus.Subscription{
		{Durable: "places-v4", Type: events.TripRequested, AckWait: 5 * time.Minute, MaxDeliver: 3, Handler: s.handlePlaces},
		{Durable: "images-v4", Type: events.TripRequested, AckWait: 30 * time.Second, MaxDeliver: 3, Handler: s.handleImages},
	}
}

var placeCategories = struct{ hotels, restaurants, attractions string }{
	hotels:      "accommodation.hotel",
	restaurants: "catering.restaurant",
	attractions: "tourism.attraction",
}

func (s *Service) handlePlaces(ctx context.Context, e bus.Event) error {
	var trip types.TripRequest
	if err := e.Decode(&trip); err != nil {
		return err
	}
	log := s.log.With("trip_id", trip.RequestID, "destination", trip.Destination)

	lat, lon, err := s.geo.GetCoordinates(trip.Destination)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			// Retrying cannot make a place exist. Publish an empty result so the
			// trip can still finish instead of waiting out the stream's grace period.
			log.Warn("destination could not be geocoded", "error", err)
			return s.publishPlaces(ctx, trip.RequestID, events.PlaceGroups{})
		}
		log.Warn("geocoding failed; will retry", "error", err)
		return fmt.Errorf("geocode %q: %w", trip.Destination, err)
	}

	// Each category is fetched on its own: one failing must not turn into a null
	// column, and every one failing means the trip would have no places at all, so
	// that case is retried instead of published.
	search := func(category string) ([]geo.Place, bool) {
		places, err := s.geo.SearchPlaces(lat, lon, category, 5)
		if err != nil {
			log.Warn("place lookup failed", "category", category, "error", err)
			return []geo.Place{}, false
		}
		if len(places) == 0 {
			log.Info("place lookup returned nothing", "category", category)
		}
		return places, true
	}
	hotels, okH := search(placeCategories.hotels)
	restaurants, okR := search(placeCategories.restaurants)
	attractions, okA := search(placeCategories.attractions)
	if !okH && !okR && !okA {
		return fmt.Errorf("every place lookup failed for %q", trip.Destination)
	}

	return s.publishPlaces(ctx, trip.RequestID, events.PlaceGroups{Hotels: hotels, Restaurants: restaurants, Attractions: attractions})
}

func (s *Service) publishPlaces(ctx context.Context, requestID string, groups events.PlaceGroups) error {
	out, err := bus.NewEvent(events.TripPlaces, events.ID(events.TripPlaces, requestID), events.PlacesFound{
		RequestID: requestID, Places: groups,
	})
	if err != nil {
		return err
	}
	if err := s.pub.Publish(ctx, out); err != nil {
		return fmt.Errorf("publish places: %w", err)
	}
	s.log.Info("places found", "trip_id", requestID, "count", len(groups.All()))
	return nil
}

func (s *Service) handleImages(ctx context.Context, e bus.Event) error {
	var trip types.TripRequest
	if err := e.Decode(&trip); err != nil {
		return err
	}
	imgs, err := s.images.GetImages(ctx, trip.Destination)
	if err != nil {
		s.log.Warn("image lookup failed; will retry", "trip_id", trip.RequestID, "error", err)
		return fmt.Errorf("images for %q: %w", trip.Destination, err)
	}
	out, err := bus.NewEvent(events.TripImages, events.ID(events.TripImages, trip.RequestID), events.ImagesFound{
		RequestID: trip.RequestID, Images: imgs,
	})
	if err != nil {
		return err
	}
	if err := s.pub.Publish(ctx, out); err != nil {
		return fmt.Errorf("publish images: %w", err)
	}
	s.log.Info("images found", "trip_id", trip.RequestID, "count", len(imgs))
	return nil
}
