package geo

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGetCoordinates(t *testing.T) {
	// Mock Nominatim
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			query := r.URL.Query().Get("q")
			if query == "Paris" {
				w.Write([]byte(`[{"lat": "48.8566", "lon": "2.3522"}]`))
				return
			}
			w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	svc := NewService("")
	svc.nominatimURL = server.URL
	// Use custom client with very short timeout
	svc.client = server.Client()

	lat, lon, err := svc.GetCoordinates("Paris")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if lat != 48.8566 || lon != 2.3522 {
		t.Errorf("expected 48.8566, 2.3522, got %f, %f", lat, lon)
	}

	_, _, err = svc.GetCoordinates("UnknownCityName")
	if err == nil {
		t.Fatalf("expected error for unknown city, got nil")
	}
}

func TestSearchPlaces(t *testing.T) {
	// Mock Overpass
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"elements": [
				{
					"lat": 48.8566,
					"lon": 2.3522,
					"tags": {
						"name": "Eiffel Tower",
						"addr:street": "Champ de Mars"
					},
					"type": "node"
				}
			]
		}`))
	}))
	defer server.Close()

	svc := NewService("")
	svc.overpassURL = server.URL
	svc.overpassClient = server.Client()

	places, err := svc.SearchPlaces(48.8566, 2.3522, "tourism.attraction", 5)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(places) != 1 {
		t.Fatalf("expected 1 place, got %d", len(places))
	}

	if places[0].Name != "Eiffel Tower" {
		t.Errorf("expected Eiffel Tower, got %s", places[0].Name)
	}
}

// Overpass answers 504 with an HTML body when a query exceeds its time budget.
// That must surface as an error rather than an empty, error-free result, which
// is what used to let the places worker publish a null column.
func TestSearchPlacesSurfacesHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("<html><body>Error: runtime error: Query timed out</body></html>"))
	}))
	defer server.Close()

	svc := NewService("")
	svc.overpassURL = server.URL
	svc.overpassClient = server.Client()

	places, err := svc.SearchPlaces(48.8566, 2.3522, "tourism.attraction", 5)
	if err == nil {
		t.Fatalf("expected an error for a non-200 Overpass response, got %d places", len(places))
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("expected the status code in the error, got %v", err)
	}
	if places != nil {
		t.Errorf("expected no places alongside the error, got %v", places)
	}
}

func TestSearchPlacesRetriesTransientStatus(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusGatewayTimeout)
			w.Write([]byte("<html>gateway timeout</html>"))
			return
		}
		w.Write([]byte(`{"elements":[{"lat":48.85,"lon":2.35,"type":"node","tags":{"name":"Louvre","tourism":"museum"}}]}`))
	}))
	defer server.Close()

	svc := NewService("")
	svc.overpassURL = server.URL
	svc.overpassClient = server.Client()

	places, err := svc.SearchPlaces(48.8566, 2.3522, "tourism.attraction", 5)
	if err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if len(places) != 1 || places[0].Name != "Louvre" {
		t.Fatalf("expected the Louvre from the second attempt, got %v", places)
	}
	if places[0].Category != "tourism.museum" {
		t.Errorf("expected category tourism.museum, got %q", places[0].Category)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("expected 2 Overpass calls, got %d", got)
	}
}

// The Overpass request budget has to be long enough that dense cities finish
// server-side; 15s is what made Tokyo and Paris return 504.
func TestOverpassQueryUsesGenerousTimeout(t *testing.T) {
	q := buildOverpassQuery(35.68, 139.76, "accommodation.hotel", 5)
	if !strings.Contains(q, "[timeout:50]") {
		t.Errorf("expected a 50s Overpass timeout, got query:\n%s", q)
	}
	if !strings.Contains(q, `["name"]`) {
		t.Errorf("expected the query to require a name tag, got query:\n%s", q)
	}
}
