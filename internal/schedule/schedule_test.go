package schedule

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sky-walkerx/trequila/internal/ors"
)

var lisbon, _ = time.LoadLocation("Europe/Lisbon")

// Monday 2026-12-07
var monday = time.Date(2026, 12, 7, 0, 0, 0, 0, lisbon)

// keyless ORS: estimates from straight-line distance, which is what we want for geometry here.
var prov = ors.New(ors.Config{})

// onLine places stops along a street: stop i is i*step degrees of longitude east (~430 m at 0.005).
func sight(id string, i int, step float64, hours string) Stop {
	return Stop{ItemID: "item-" + id, PlaceID: "node/" + id, Name: id, Kind: "sight", Category: "attraction",
		Lat: 38.7, Lon: -9.15 + float64(i)*step, OpeningHours: hours}
}

func meal(id, slot string, i int) Stop {
	return Stop{ItemID: "item-" + id, PlaceID: "node/" + id, Name: id, Kind: "meal", Category: "restaurant", Slot: slot,
		Lat: 38.7, Lon: -9.15 + float64(i)*0.005}
}

func order(d Day) string {
	var ids []string
	for _, v := range d.Visits {
		ids = append(ids, strings.TrimPrefix(v.ItemID, "item-"))
	}
	return strings.Join(ids, ",")
}

func plan(t *testing.T, stops []Stop) Day {
	t.Helper()
	d, err := PlanDay(context.Background(), prov, stops, Params{Date: monday})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSightsAreVisitedInAWalkingOrderNotTheOrderTheyWereGiven(t *testing.T) {
	// Ranked A..D, but laid out along the street as A, C, B, D... start from A (best ranked).
	stops := []Stop{sight("A", 0, 0.005, ""), sight("D", 3, 0.005, ""), sight("B", 1, 0.005, ""), sight("C", 2, 0.005, "")}
	d := plan(t, stops)
	if order(d) != "A,B,C,D" {
		t.Errorf("order = %s", order(d))
	}
	if d.Mode != "walk" || !d.Estimated {
		t.Errorf("mode=%s estimated=%v; short legs without a routing key are walked and labelled estimated", d.Mode, d.Estimated)
	}
	if d.Visits[0].Arrive.Hour() != 9 || d.Visits[0].Arrive.Minute() != 0 {
		t.Errorf("the day starts at 09:00, got %s", d.Visits[0].Arrive)
	}
	if d.Visits[1].LegDurationS <= 0 || d.Visits[1].LegDistanceM <= 0 || d.Visits[0].LegDurationS != 0 {
		t.Errorf("legs: %+v", d.Visits)
	}
}

func TestLongLegsSwitchTheDayToDriving(t *testing.T) {
	d := plan(t, []Stop{sight("A", 0, 0.05, ""), sight("B", 1, 0.05, ""), sight("C", 2, 0.05, "")}) // ~4.3 km apart
	if d.Mode != "drive" {
		t.Errorf("mode = %s, want drive", d.Mode)
	}
}

func TestAPlaceThatOpensLaterIsVisitedLaterNotWaitedFor(t *testing.T) {
	late := sight("L", 1, 0.005, "Mo-Su 14:00-18:00")
	d := plan(t, []Stop{sight("A", 0, 0.005, ""), late, sight("C", 2, 0.005, ""), sight("D", 3, 0.005, "")})

	if len(d.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v", d.Conflicts)
	}
	for _, v := range d.Visits {
		if v.ItemID == "item-L" && v.Arrive.Hour() < 14 {
			t.Errorf("L visited at %s, before it opens", v.Arrive.Format("15:04"))
		}
	}
}

func TestAShortWaitForOpeningIsAcceptedAndRecorded(t *testing.T) {
	// Only stop of the day, opens at 09:30: arrive 09:00, wait 30 min.
	d := plan(t, []Stop{sight("A", 0, 0.005, "Mo-Su 09:30-18:00")})
	v := d.Visits[0]
	if len(d.Conflicts) != 0 || v.Arrive.Format("15:04") != "09:30" || v.WaitS != 1800 {
		t.Errorf("visit = %+v conflicts = %+v", v, d.Conflicts)
	}
}

func TestAPlaceClosedOnThisWeekdayIsFlaggedNeverSilentlyScheduled(t *testing.T) {
	d := plan(t, []Stop{sight("A", 0, 0.005, ""), sight("X", 1, 0.005, "Sa,Su 10:00-18:00")})

	if len(d.Conflicts) != 1 || d.Conflicts[0].ItemID != "item-X" || d.Conflicts[0].Kind != ConflictClosedToday {
		t.Fatalf("conflicts = %+v", d.Conflicts)
	}
	if !strings.Contains(d.Conflicts[0].Message, "closed") {
		t.Errorf("message = %q", d.Conflicts[0].Message)
	}
	if order(d) != "A,X" {
		t.Errorf("the closed place still appears, flagged: %s", order(d))
	}
}

func TestAVisitThatCannotFinishBeforeClosingIsFlagged(t *testing.T) {
	// Museum (90 min) closing at 09:45 when we arrive at 09:00.
	m := sight("M", 0, 0.005, "Mo-Su 08:00-09:45")
	m.Category = "museum"
	d := plan(t, []Stop{m})
	if len(d.Conflicts) != 1 || d.Conflicts[0].Kind != ConflictClosesBeforeEnd {
		t.Errorf("conflicts = %+v", d.Conflicts)
	}
}

func TestUnknownOrUnparseableHoursNeverConflict(t *testing.T) {
	d := plan(t, []Stop{sight("A", 0, 0.005, ""), sight("B", 1, 0.005, "sunrise-sunset"), sight("C", 2, 0.005, "by appointment")})
	if len(d.Conflicts) != 0 {
		t.Errorf("conflicts = %+v", d.Conflicts)
	}
	for _, v := range d.Visits {
		if v.Hours != HoursUnknown {
			t.Errorf("%s hours = %q, want unknown", v.ItemID, v.Hours)
		}
	}
}

func TestMealsLandInTheirWindows(t *testing.T) {
	stops := []Stop{sight("A", 0, 0.005, ""), sight("B", 1, 0.005, ""), sight("C", 2, 0.005, ""), sight("D", 3, 0.005, ""),
		meal("L", "lunch", 2), meal("N", "dinner", 3)}
	d := plan(t, stops)

	var lunch, dinner *Visit
	for i := range d.Visits {
		switch d.Visits[i].ItemID {
		case "item-L":
			lunch = &d.Visits[i]
		case "item-N":
			dinner = &d.Visits[i]
		}
	}
	if lunch == nil || dinner == nil {
		t.Fatalf("meals missing: %s", order(d))
	}
	if h := lunch.Arrive.Hour(); h < 11 || h > 14 {
		t.Errorf("lunch at %s", lunch.Arrive.Format("15:04"))
	}
	if dinner.Arrive.Hour() < 18 {
		t.Errorf("dinner at %s", dinner.Arrive.Format("15:04"))
	}
	if order(d)[len(order(d))-1] != 'N' {
		t.Errorf("dinner should close the day: %s", order(d))
	}
}

func TestDaysAreSimulatedInTheDestinationsLocalTime(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	dstEnd := time.Date(2026, 11, 1, 0, 0, 0, 0, ny) // clocks go back at 02:00
	s := sight("A", 0, 0.005, "Mo-Su 09:00-12:00,13:00-17:00")
	s.Lat, s.Lon = 40.7, -74.0
	b := s
	b.ItemID, b.PlaceID, b.Name, b.Lon = "item-B", "node/B", "B", -74.0+0.005

	d, err := PlanDay(context.Background(), prov, []Stop{s, b}, Params{Date: dstEnd})
	if err != nil {
		t.Fatal(err)
	}
	if d.Visits[0].Arrive.Format("15:04 MST") != "09:00 EST" {
		t.Errorf("start = %s", d.Visits[0].Arrive.Format("15:04 MST"))
	}
	if len(d.Conflicts) != 0 {
		t.Errorf("conflicts = %+v", d.Conflicts)
	}
}

func TestPlanningIsDeterministic(t *testing.T) {
	stops := []Stop{sight("A", 0, 0.005, ""), sight("B", 3, 0.005, "Mo-Su 10:00-12:00"), sight("C", 1, 0.005, ""), sight("D", 2, 0.005, "Tu-Su 09:00-18:00"),
		meal("L", "lunch", 1), meal("N", "dinner", 2)}
	first := plan(t, stops)
	for i := 0; i < 5; i++ {
		if again := plan(t, stops); !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs:\n%s\n%s", i, order(first), order(again))
		}
	}
}

func TestEveryStopAppearsExactlyOnce(t *testing.T) {
	var stops []Stop
	for i := 0; i < 7; i++ {
		stops = append(stops, sight(fmt.Sprint(i), i, 0.004, ""))
	}
	stops = append(stops, meal("L", "lunch", 3), meal("N", "dinner", 5))
	d := plan(t, stops)
	if len(d.Visits) != 9 {
		t.Fatalf("visits = %d", len(d.Visits))
	}
	seen := map[string]bool{}
	for _, v := range d.Visits {
		if seen[v.ItemID] {
			t.Errorf("%s twice", v.ItemID)
		}
		seen[v.ItemID] = true
	}
}

func TestAnEmptyDayIsFine(t *testing.T) {
	d := plan(t, nil)
	if len(d.Visits) != 0 || len(d.Conflicts) != 0 {
		t.Errorf("day = %+v", d)
	}
}
