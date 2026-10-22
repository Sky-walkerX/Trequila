package events_test

import (
	"encoding/json"
	"testing"

	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/types"
)

func TestIDIsDeterministicPerTypeAndTrip(t *testing.T) {
	if got := events.ID(events.TripItinerary, "t-1"); got != "trips.itinerary:t-1" {
		t.Errorf("ID = %q", got)
	}
	first, second := events.ID(events.TripItinerary, "t-1"), events.ID(events.TripItinerary, "t-1")
	if first != second {
		t.Error("ID must be stable so redelivered work de-duplicates")
	}
}

func TestPayloadsKeepTheWireFieldNamesClientsAndConsumersRelyOn(t *testing.T) {
	b, _ := json.Marshal(events.PlacesFound{
		RequestID: "t-1",
		Places:    events.PlaceGroups{Attractions: []geo.Place{{Name: "Senso-ji"}}},
	})
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["request_id"] != "t-1" {
		t.Errorf("request_id missing: %s", b)
	}
	groups, _ := m["places"].(map[string]any)
	for _, k := range []string{"hotels", "restaurants", "attractions"} {
		if _, ok := groups[k]; !ok {
			t.Errorf("places.%s missing: %s", k, b)
		}
	}
	// Empty groups are [] not null: a null here once starved a downstream consumer.
	if groups["hotels"] == nil {
		t.Errorf("hotels should marshal as an empty array, not null: %s", b)
	}

	b, _ = json.Marshal(events.ItineraryGenerated{RequestID: "t-1", Itinerary: []types.DayPlan{{DayIndex: 0}}})
	json.Unmarshal(b, &m)
	if m["request_id"] == nil || m["itinerary"] == nil {
		t.Errorf("itinerary payload keys wrong: %s", b)
	}
}
