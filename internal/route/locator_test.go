package route

import (
	"errors"
	"testing"

	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/types"
)

type fakeGeocoder struct {
	answers map[string]Point
	calls   []string
}

func (f *fakeGeocoder) GetCoordinates(query string) (float64, float64, error) {
	f.calls = append(f.calls, query)
	if p, ok := f.answers[query]; ok {
		return p.Lat, p.Lon, nil
	}
	return 0, 0, errors.New("location not found")
}

var tokyoPlaces = []geo.Place{
	{Name: "Senso-ji", Lat: 35.7148, Lon: 139.7967},
	{Name: "Tokyo Skytree", Lat: 35.7101, Lon: 139.8107},
	{Name: "No coordinates café", Lat: 0, Lon: 0},
}

func TestKnownPlaceIsMatchedWithoutGeocoding(t *testing.T) {
	geocoder := &fakeGeocoder{}
	l := NewPlaceLocator("Tokyo", tokyoPlaces, geocoder)

	got, ok := l.Locate(types.Activity{Name: "Tokyo Skytree"})
	if !ok {
		t.Fatal("known place was not matched")
	}
	if got.Lat != 35.7101 {
		t.Fatalf("lat = %v, want 35.7101", got.Lat)
	}
	if got.Geocoded {
		t.Error("a place taken from the trip should not be flagged as geocoded")
	}
	if len(geocoder.calls) != 0 {
		t.Fatalf("geocoder was called %v; the place was already known", geocoder.calls)
	}
}

func TestPlaceNamesMatchDespitePunctuationAndCase(t *testing.T) {
	l := NewPlaceLocator("Tokyo", tokyoPlaces, &fakeGeocoder{})
	if _, ok := l.Locate(types.Activity{Name: "  senso-ji!  "}); !ok {
		t.Fatal("punctuation and case should not prevent a match")
	}
}

func TestQualifiedActivityNameMatchesTheUnderlyingPlace(t *testing.T) {
	l := NewPlaceLocator("Tokyo", tokyoPlaces, &fakeGeocoder{})
	if _, ok := l.Locate(types.Activity{Name: "Visit Tokyo Skytree observation deck"}); !ok {
		t.Fatal("an activity naming a known place should match it")
	}
}

func TestPlacesWithoutCoordinatesAreNotMatched(t *testing.T) {
	l := NewPlaceLocator("Tokyo", tokyoPlaces, nil)
	if _, ok := l.Locate(types.Activity{Name: "No coordinates café"}); ok {
		t.Fatal("a 0,0 place is missing coordinates, not located off West Africa")
	}
}

func TestUnknownActivityFallsBackToGeocodingQualifiedByDestination(t *testing.T) {
	geocoder := &fakeGeocoder{answers: map[string]Point{
		"Blue Note, Tokyo": {Lat: 35.66, Lon: 139.71},
	}}
	l := NewPlaceLocator("Tokyo", tokyoPlaces, geocoder)

	got, ok := l.Locate(types.Activity{Name: "Blue Note"})
	if !ok {
		t.Fatalf("geocoder fallback failed; calls were %v", geocoder.calls)
	}
	if !got.Geocoded {
		t.Error("a geocoded point should be flagged as such")
	}
	if geocoder.calls[0] != "Blue Note, Tokyo" {
		t.Fatalf("query = %q, want it qualified by the destination", geocoder.calls[0])
	}
}

func TestAddressIsPreferredOverTheActivityName(t *testing.T) {
	geocoder := &fakeGeocoder{answers: map[string]Point{
		"6-chome, Ginza, Tokyo": {Lat: 35.67, Lon: 139.76},
	}}
	l := NewPlaceLocator("Tokyo", nil, geocoder)

	if _, ok := l.Locate(types.Activity{Name: "Dinner", Address: "6-chome, Ginza, Tokyo"}); !ok {
		t.Fatalf("address lookup failed; calls were %v", geocoder.calls)
	}
}

func TestAFailedLookupIsNotRepeated(t *testing.T) {
	geocoder := &fakeGeocoder{}
	l := NewPlaceLocator("Tokyo", nil, geocoder)

	activity := types.Activity{Name: "Wander around aimlessly"}
	for i := 0; i < 3; i++ {
		if _, ok := l.Locate(activity); ok {
			t.Fatal("prose is not a place")
		}
	}
	if len(geocoder.calls) != 1 {
		t.Fatalf("geocoder called %d times, want 1 — negatives must be memoised", len(geocoder.calls))
	}
}

func TestASuccessfulLookupIsNotRepeated(t *testing.T) {
	geocoder := &fakeGeocoder{answers: map[string]Point{"Blue Note, Tokyo": {Lat: 35.66, Lon: 139.71}}}
	l := NewPlaceLocator("Tokyo", nil, geocoder)

	for i := 0; i < 3; i++ {
		if _, ok := l.Locate(types.Activity{Name: "Blue Note"}); !ok {
			t.Fatal("lookup failed")
		}
	}
	if len(geocoder.calls) != 1 {
		t.Fatalf("geocoder called %d times, want 1", len(geocoder.calls))
	}
}

func TestGeocodingIsBudgetedPerJob(t *testing.T) {
	geocoder := &fakeGeocoder{}
	l := NewPlaceLocator("Tokyo", nil, geocoder)

	// Each name is distinct, so the memo cannot absorb these.
	for i := 0; i < maxGeocodeLookups+10; i++ {
		l.Locate(types.Activity{Name: string(rune('a'+i%26)) + "ctivity number " + string(rune('0'+i%10)) + string(rune('a'+i/26))})
	}
	if len(geocoder.calls) > maxGeocodeLookups {
		t.Fatalf("%d geocoder calls exceeds the budget of %d", len(geocoder.calls), maxGeocodeLookups)
	}
}

func TestNoGeocoderStillMatchesKnownPlaces(t *testing.T) {
	l := NewPlaceLocator("Tokyo", tokyoPlaces, nil)
	if _, ok := l.Locate(types.Activity{Name: "Senso-ji"}); !ok {
		t.Fatal("known place should match without a geocoder")
	}
	if _, ok := l.Locate(types.Activity{Name: "Somewhere else"}); ok {
		t.Fatal("unknown place cannot resolve without a geocoder")
	}
}

func TestBlankActivityNameIsNotLocated(t *testing.T) {
	geocoder := &fakeGeocoder{}
	l := NewPlaceLocator("Tokyo", tokyoPlaces, geocoder)
	if _, ok := l.Locate(types.Activity{Name: "   "}); ok {
		t.Fatal("a blank name is not a place")
	}
	if len(geocoder.calls) != 0 {
		t.Fatal("a blank name should not reach the geocoder")
	}
}

func TestShortNamesRequireAnExactMatch(t *testing.T) {
	places := []geo.Place{{Name: "Bar", Lat: 1, Lon: 2}}
	l := NewPlaceLocator("Tokyo", places, nil)

	if _, ok := l.Locate(types.Activity{Name: "Bar"}); !ok {
		t.Fatal("exact short name should match")
	}
	// "Barbecue lunch" contains "bar"; matching it would be nonsense.
	if _, ok := l.Locate(types.Activity{Name: "Barbecue lunch"}); ok {
		t.Fatal("short place names must not match by substring")
	}
}
