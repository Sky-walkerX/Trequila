package route

import (
	"strings"
	"time"
	"unicode"

	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/types"
)

// maxGeocodeLookups caps how many activities one job will send to the geocoder.
// Nominatim is rate-limited to roughly one request per second, so an uncapped
// job on a long trip would run for minutes and mostly on names that are prose
// ("Dinner somewhere in Shibuya") rather than places.
const maxGeocodeLookups = 40

// Geocoder resolves a free-text query to coordinates. geo.Service satisfies it.
type Geocoder interface {
	GetCoordinates(query string) (float64, float64, error)
}

// PlaceLocator resolves an activity to coordinates, preferring the places
// already attached to the trip — those were found near the destination and cost
// nothing — and falling back to geocoding the activity by name.
type PlaceLocator struct {
	destination string
	known       map[string]Point
	geocoder    Geocoder
	budget      int
	deadline    time.Time        // zero means no deadline
	resolved    map[string]Point // memo, including negatives, for one job
	failed      map[string]bool
}

// WithDeadline stops the locator geocoding past t, leaving the remaining
// activities unlocated. Lookups are rate-limited to roughly one per second, so
// without a wall-clock bound a slow geocoder would hold a job open long enough
// for JetStream to redeliver it mid-flight.
func (l *PlaceLocator) WithDeadline(t time.Time) *PlaceLocator {
	l.deadline = t
	return l
}

// NewPlaceLocator builds a locator over the trip's own places. geocoder may be
// nil, in which case only the known places are matched.
func NewPlaceLocator(destination string, places []geo.Place, geocoder Geocoder) *PlaceLocator {
	known := make(map[string]Point, len(places))
	for _, p := range places {
		if p.Lat == 0 && p.Lon == 0 {
			continue
		}
		if key := normalise(p.Name); key != "" {
			known[key] = Point{Lat: p.Lat, Lon: p.Lon}
		}
	}
	return &PlaceLocator{
		destination: destination,
		known:       known,
		geocoder:    geocoder,
		budget:      maxGeocodeLookups,
		resolved:    map[string]Point{},
		failed:      map[string]bool{},
	}
}

// Locate implements Locator.
func (l *PlaceLocator) Locate(activity types.Activity) (Point, bool) {
	key := normalise(activity.Name)
	if key == "" {
		return Point{}, false
	}
	if p, ok := l.resolved[key]; ok {
		return p, true
	}
	if l.failed[key] {
		return Point{}, false
	}

	if p, ok := l.matchKnown(key); ok {
		l.resolved[key] = p
		return p, true
	}

	p, ok := l.geocode(activity)
	if !ok {
		l.failed[key] = true
		return Point{}, false
	}
	l.resolved[key] = p
	return p, true
}

// matchKnown looks the activity up among the trip's places, allowing one name to
// contain the other so "Senso-ji Temple" matches a place called "Senso-ji".
// Short keys are matched exactly, because a two-letter substring matches almost
// anything.
func (l *PlaceLocator) matchKnown(key string) (Point, bool) {
	if p, ok := l.known[key]; ok {
		return p, true
	}
	if len(key) < 6 {
		return Point{}, false
	}
	for name, p := range l.known {
		if len(name) < 6 {
			continue
		}
		if strings.Contains(key, name) || strings.Contains(name, key) {
			return p, true
		}
	}
	return Point{}, false
}

// geocode asks the geocoder, qualifying the query with the destination so that
// "Central Park" does not resolve to a different city's.
func (l *PlaceLocator) geocode(activity types.Activity) (Point, bool) {
	if l.geocoder == nil || l.budget <= 0 {
		return Point{}, false
	}
	if !l.deadline.IsZero() && time.Now().After(l.deadline) {
		return Point{}, false
	}

	query := strings.TrimSpace(activity.Address)
	if query == "" {
		query = strings.TrimSpace(activity.Name)
		if l.destination != "" {
			query += ", " + l.destination
		}
	}
	if query == "" {
		return Point{}, false
	}

	l.budget--
	lat, lon, err := l.geocoder.GetCoordinates(query)
	if err != nil || (lat == 0 && lon == 0) {
		return Point{}, false
	}
	return Point{Lat: lat, Lon: lon, Geocoded: true}, true
}

// normalise lowercases and strips punctuation so that "Sensō-ji Temple," and
// "sensoji temple" compare equal.
func normalise(name string) string {
	var b strings.Builder
	lastWasSpace := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastWasSpace = false
		case !lastWasSpace:
			b.WriteRune(' ')
			lastWasSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}
