package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/route"
	"github.com/sky-walkerx/trequila/internal/types"
)

// The optimize endpoint sits under the "/api/trips/" prefix that handleTripByID
// already claims. If ServeMux ever stopped preferring the longer pattern, an
// optimize request would fall through to handleTripByID and 405, which is
// exactly the bug this endpoint was added to fix.
func TestOptimizePatternWinsOverTheTripsPrefix(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/trips/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("trips"))
	})
	mux.HandleFunc("/api/trips/optimize/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("optimize"))
	})

	for path, want := range map[string]string{
		"/api/trips/optimize/abc-123": "optimize",
		"/api/trips/abc-123":          "trips",
		"/api/trips/stream/abc-123":   "trips",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if got := rec.Body.String(); got != want {
			t.Errorf("%s routed to %q, want %q", path, got, want)
		}
	}
}

func TestFlattenPlacesReadsTheStoredGroupedShape(t *testing.T) {
	stored := []byte(`{
		"hotels":      [{"name":"Park Hyatt","lat":35.6857,"lon":139.6907}],
		"restaurants": [{"name":"Sukiyabashi","lat":35.6717,"lon":139.7638}],
		"attractions": [{"name":"Senso-ji","lat":35.7148,"lon":139.7967}]
	}`)

	got := flattenPlaces(stored)

	if len(got) != 3 {
		t.Fatalf("flattened %d places, want 3", len(got))
	}
	byName := map[string]geo.Place{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if byName["Senso-ji"].Lat != 35.7148 {
		t.Errorf("Senso-ji lat = %v, want 35.7148", byName["Senso-ji"].Lat)
	}
}

func TestFlattenPlacesAcceptsAFlatArray(t *testing.T) {
	got := flattenPlaces([]byte(`[{"name":"Senso-ji","lat":35.7148,"lon":139.7967}]`))
	if len(got) != 1 || got[0].Name != "Senso-ji" {
		t.Fatalf("got %+v, want one Senso-ji", got)
	}
}

func TestFlattenPlacesToleratesEmptyAndBrokenInput(t *testing.T) {
	for name, raw := range map[string][]byte{
		"nil":     nil,
		"empty":   {},
		"null":    []byte(`null`),
		"object":  []byte(`{}`),
		"garbage": []byte(`not json`),
		"scalar":  []byte(`42`),
	} {
		if got := flattenPlaces(raw); len(got) != 0 {
			t.Errorf("%s gave %+v, want nothing", name, got)
		}
	}
}

// The route JSON is rendered by the frontend without a translation layer, so
// the wire field names are a contract. This pins them.
func TestRouteJSONMatchesTheFrontendContract(t *testing.T) {
	built := route.Build(
		[]types.DayPlan{{DayIndex: 0, Activities: []types.Activity{
			{Name: "Senso-ji", Time: "09:00"},
			{Name: "Skytree", Time: "11:00"},
		}}},
		route.LocatorFunc(func(a types.Activity) (route.Point, bool) {
			switch a.Name {
			case "Senso-ji":
				return route.Point{Lat: 35.7148, Lon: 139.7967}, true
			case "Skytree":
				return route.Point{Lat: 35.7101, Lon: 139.8107, Geocoded: true}, true
			}
			return route.Point{}, false
		}),
	)

	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	requireKeys(t, decoded, "generated_at", "days")

	days, ok := decoded["days"].([]any)
	if !ok || len(days) != 1 {
		t.Fatalf("days = %v, want one entry", decoded["days"])
	}
	day := days[0].(map[string]any)
	requireKeys(t, day, "dayIndex", "mode", "ordered", "legs", "unlocated", "total_distance_m", "total_duration_s")

	if mode := day["mode"].(string); mode != "walk" && mode != "drive" {
		t.Errorf(`mode = %q, want "walk" or "drive"`, mode)
	}

	ordered := day["ordered"].([]any)
	if len(ordered) != 2 {
		t.Fatalf("ordered %d stops, want 2", len(ordered))
	}
	requireKeys(t, ordered[0].(map[string]any), "activityRef", "name", "lat", "lon", "time", "geocoded")

	legs := day["legs"].([]any)
	if len(legs) != 1 {
		t.Fatalf("%d legs, want 1", len(legs))
	}
	requireKeys(t, legs[0].(map[string]any), "fromRef", "toRef", "distance_m", "duration_s", "mode")
}

// An empty day must serialise its arrays as [] rather than null: the frontend
// calls .length and .find on them without guarding.
func TestEmptyRouteDaySerialisesArraysNotNull(t *testing.T) {
	built := route.Build(
		[]types.DayPlan{{DayIndex: 0, Activities: []types.Activity{{Name: "unplaceable"}}}},
		route.LocatorFunc(func(types.Activity) (route.Point, bool) { return route.Point{}, false }),
	)

	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Days []struct {
			Ordered   *[]any `json:"ordered"`
			Legs      *[]any `json:"legs"`
			Unlocated *[]int `json:"unlocated"`
		} `json:"days"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	day := decoded.Days[0]
	if day.Ordered == nil || day.Legs == nil || day.Unlocated == nil {
		t.Fatalf("a null array reached the wire: %s", raw)
	}
	if len(*day.Unlocated) != 1 || (*day.Unlocated)[0] != 0 {
		t.Errorf("unlocated = %v, want [0]", day.Unlocated)
	}
}

func requireKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in %v", k, m)
		}
	}
}
