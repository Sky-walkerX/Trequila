package discovery_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/bus/memory"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/images"
	"github.com/sky-walkerx/trequila/internal/svc/discovery"
	"github.com/sky-walkerx/trequila/internal/types"
)

type fakeGeo struct {
	coordsErr error
	byCat     map[string][]geo.Place
	catErr    map[string]error
}

func (f fakeGeo) GetCoordinates(string) (float64, float64, error) { return 38.7, -9.1, f.coordsErr }
func (f fakeGeo) SearchPlaces(_, _ float64, category string, _ int) ([]geo.Place, error) {
	if err := f.catErr[category]; err != nil {
		return nil, err
	}
	return f.byCat[category], nil
}

type fakeImages struct {
	imgs []images.Image
	err  error
}

func (f fakeImages) GetImages(context.Context, string) ([]images.Image, error) { return f.imgs, f.err }

func request(t *testing.T) bus.Event {
	t.Helper()
	e, _ := bus.NewEvent(events.TripRequested, events.ID(events.TripRequested, "t-1"), types.TripRequest{
		RequestID: "t-1", UserID: "u1", Destination: "Lisbon",
	})
	return e
}

func handlers(t *testing.T, s *discovery.Service) (places, imgs bus.Handler) {
	t.Helper()
	for _, sub := range s.Subscriptions() {
		if sub.Type != events.TripRequested {
			t.Fatalf("unexpected subscription type %q", sub.Type)
		}
		switch sub.Durable {
		case "places-v4":
			places = sub.Handler
		case "images-v4":
			imgs = sub.Handler
		}
	}
	if places == nil || imgs == nil {
		t.Fatal("expected a places-v4 and an images-v4 subscription")
	}
	return
}

func goodGeo() fakeGeo {
	return fakeGeo{byCat: map[string][]geo.Place{
		"accommodation.hotel": {{Name: "Hotel A"}},
		"catering.restaurant": {{Name: "Cervejaria"}},
		"tourism.attraction":  {{Name: "Belem Tower"}},
	}}
}

func decodePlaces(t *testing.T, b *memory.Bus) events.PlacesFound {
	t.Helper()
	evs := b.Events()
	if len(evs) != 1 || evs[0].Type != events.TripPlaces || evs[0].ID != events.ID(events.TripPlaces, "t-1") {
		t.Fatalf("published = %+v", evs)
	}
	var got events.PlacesFound
	if err := evs[0].Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPlacesAreGroupedAndPublished(t *testing.T) {
	b := memory.New()
	places, _ := handlers(t, discovery.New(goodGeo(), fakeImages{}, b))

	if err := places(context.Background(), request(t)); err != nil {
		t.Fatal(err)
	}

	got := decodePlaces(t, b)
	if got.RequestID != "t-1" || len(got.Places.Hotels) != 1 || got.Places.Restaurants[0].Name != "Cervejaria" || got.Places.Attractions[0].Name != "Belem Tower" {
		t.Errorf("payload = %+v", got)
	}
}

func TestADestinationThatCannotBeFoundPublishesEmptyPlacesSoTheTripCanFinish(t *testing.T) {
	b := memory.New()
	g := goodGeo()
	g.coordsErr = errors.New("location not found")
	places, _ := handlers(t, discovery.New(g, fakeImages{}, b))

	if err := places(context.Background(), request(t)); err != nil {
		t.Fatalf("a destination that does not exist is not retryable, and not an error: %v", err)
	}

	got := decodePlaces(t, b)
	if len(got.Places.All()) != 0 {
		t.Errorf("expected no places, got %+v", got.Places)
	}
}

func TestATransientGeocodingErrorIsRetried(t *testing.T) {
	b := memory.New()
	g := goodGeo()
	g.coordsErr = errors.New("connection reset")
	places, _ := handlers(t, discovery.New(g, fakeImages{}, b))

	err := places(context.Background(), request(t))

	if err == nil || bus.IsPermanent(err) {
		t.Fatalf("want a retryable error, got %v", err)
	}
	if len(b.Events()) != 0 {
		t.Errorf("published %d events, want none", len(b.Events()))
	}
}

func TestOneFailingCategoryStillPublishesTheOthers(t *testing.T) {
	b := memory.New()
	g := goodGeo()
	g.catErr = map[string]error{"catering.restaurant": errors.New("overpass 504")}
	places, _ := handlers(t, discovery.New(g, fakeImages{}, b))

	if err := places(context.Background(), request(t)); err != nil {
		t.Fatal(err)
	}

	got := decodePlaces(t, b)
	if len(got.Places.Restaurants) != 0 || len(got.Places.Hotels) != 1 || len(got.Places.Attractions) != 1 {
		t.Errorf("payload = %+v", got.Places)
	}
}

func TestEveryCategoryFailingIsRetriedRatherThanPublishedAsEmpty(t *testing.T) {
	b := memory.New()
	g := goodGeo()
	boom := errors.New("overpass down")
	g.catErr = map[string]error{"accommodation.hotel": boom, "catering.restaurant": boom, "tourism.attraction": boom}
	places, _ := handlers(t, discovery.New(g, fakeImages{}, b))

	err := places(context.Background(), request(t))

	if err == nil || bus.IsPermanent(err) {
		t.Fatalf("want a retryable error, got %v", err)
	}
	if len(b.Events()) != 0 {
		t.Errorf("published %d events, want none", len(b.Events()))
	}
}

func TestImagesArePublished(t *testing.T) {
	b := memory.New()
	_, imgs := handlers(t, discovery.New(goodGeo(), fakeImages{imgs: []images.Image{{URL: "https://example.com/a.jpg"}}}, b))

	if err := imgs(context.Background(), request(t)); err != nil {
		t.Fatal(err)
	}

	evs := b.Events()
	if len(evs) != 1 || evs[0].Type != events.TripImages || evs[0].ID != events.ID(events.TripImages, "t-1") {
		t.Fatalf("published = %+v", evs)
	}
	var got events.ImagesFound
	evs[0].Decode(&got)
	if len(got.Images) != 1 || got.Images[0].URL != "https://example.com/a.jpg" {
		t.Errorf("payload = %+v", got)
	}
}

func TestAnImageLookupFailureIsRetried(t *testing.T) {
	b := memory.New()
	_, imgs := handlers(t, discovery.New(goodGeo(), fakeImages{err: errors.New("timeout")}, b))

	err := imgs(context.Background(), request(t))

	if err == nil || bus.IsPermanent(err) {
		t.Fatalf("want a retryable error, got %v", err)
	}
}
