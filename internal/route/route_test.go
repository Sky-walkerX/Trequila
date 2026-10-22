package route

import (
	"math"
	"testing"

	"github.com/sky-walkerx/trequila/internal/types"
)

// Coordinates around central Tokyo, far enough apart to be distinguishable.
var (
	shinjuku = Point{Lat: 35.6896, Lon: 139.7006}
	shibuya  = Point{Lat: 35.6580, Lon: 139.7016}
	ginza    = Point{Lat: 35.6717, Lon: 139.7650}
	asakusa  = Point{Lat: 35.7148, Lon: 139.7967}
	gyoen    = Point{Lat: 35.6852, Lon: 139.7100} // ~1km from Shinjuku, walkable
	haneda   = Point{Lat: 35.5494, Lon: 139.7798} // ~15km out, forces driving
)

// locatorFor resolves by activity name, and refuses anything not in the map.
func locatorFor(byName map[string]Point) Locator {
	return LocatorFunc(func(a types.Activity) (Point, bool) {
		p, ok := byName[a.Name]
		return p, ok
	})
}

func activities(names ...string) []types.Activity {
	out := make([]types.Activity, 0, len(names))
	for _, n := range names {
		out = append(out, types.Activity{Name: n})
	}
	return out
}

func TestHaversineMatchesKnownDistance(t *testing.T) {
	// Paris to London, ~343 km by great circle.
	got := Haversine(48.8566, 2.3522, 51.5074, -0.1278)
	if math.Abs(got-343000) > 5000 {
		t.Fatalf("Paris-London = %.0f m, want ~343000 m", got)
	}
}

func TestHaversineIsZeroForIdenticalPoints(t *testing.T) {
	if got := Haversine(35.68, 139.76, 35.68, 139.76); got != 0 {
		t.Fatalf("identical points gave %v, want 0", got)
	}
}

func TestTimedActivitiesKeepClockOrder(t *testing.T) {
	// Deliberately listed out of clock order, and out of geographic order too,
	// so a geometric sort would produce something different.
	day := types.DayPlan{
		DayIndex: 0,
		Activities: []types.Activity{
			{Name: "Asakusa", Time: "18:00"},
			{Name: "Shinjuku", Time: "09:00"},
			{Name: "Ginza", Time: "13:30"},
		},
	}
	got := buildDay(day, locatorFor(map[string]Point{
		"Asakusa": asakusa, "Shinjuku": shinjuku, "Ginza": ginza,
	}))

	want := []string{"Shinjuku", "Ginza", "Asakusa"}
	for i, name := range want {
		if got.Ordered[i].Name != name {
			t.Fatalf("stop %d = %q, want %q (full order %v)", i, got.Ordered[i].Name, name, names(got.Ordered))
		}
	}
}

func TestTwelveHourClockIsUnderstood(t *testing.T) {
	day := types.DayPlan{Activities: []types.Activity{
		{Name: "Asakusa", Time: "7:00 PM"},
		{Name: "Shinjuku", Time: "9:30 AM"},
	}}
	got := buildDay(day, locatorFor(map[string]Point{"Asakusa": asakusa, "Shinjuku": shinjuku}))
	if got.Ordered[0].Name != "Shinjuku" {
		t.Fatalf("order = %v, want Shinjuku first", names(got.Ordered))
	}
}

func TestUntimedActivitiesAreOrderedGeographically(t *testing.T) {
	// Listed so that following the itinerary order crosses the city twice.
	day := types.DayPlan{Activities: activities("Shinjuku", "Asakusa", "Shibuya", "Ginza")}
	locator := locatorFor(map[string]Point{
		"Shinjuku": shinjuku, "Asakusa": asakusa, "Shibuya": shibuya, "Ginza": ginza,
	})

	got := buildDay(day, locator)

	// The optimised tour must be no longer than walking the listed order.
	listed := tourLength([]Stop{
		{Lat: shinjuku.Lat, Lon: shinjuku.Lon},
		{Lat: asakusa.Lat, Lon: asakusa.Lon},
		{Lat: shibuya.Lat, Lon: shibuya.Lon},
		{Lat: ginza.Lat, Lon: ginza.Lon},
	})
	if got.TotalDistance >= listed {
		t.Fatalf("optimised tour %.0f m is not shorter than the listed order %.0f m", got.TotalDistance, listed)
	}
}

func TestTwoOptRemovesACrossing(t *testing.T) {
	// A→C→B→D across a square crosses itself; the optimum is the perimeter.
	square := []Stop{
		{ActivityRef: 0, Lat: 0, Lon: 0},
		{ActivityRef: 1, Lat: 0.01, Lon: 0.01},
		{ActivityRef: 2, Lat: 0, Lon: 0.01},
		{ActivityRef: 3, Lat: 0.01, Lon: 0},
	}
	before := tourLength(square)
	after := tourLength(twoOpt(square))
	if after >= before {
		t.Fatalf("twoOpt did not shorten the tour: %.1f -> %.1f", before, after)
	}
}

func TestUnlocatedActivitiesAreReportedNotDropped(t *testing.T) {
	day := types.DayPlan{Activities: activities("Shinjuku", "A vibe, not a place", "Ginza")}
	got := buildDay(day, locatorFor(map[string]Point{"Shinjuku": shinjuku, "Ginza": ginza}))

	if len(got.Ordered) != 2 {
		t.Fatalf("ordered %d stops, want 2", len(got.Ordered))
	}
	if len(got.Unlocated) != 1 || got.Unlocated[0] != 1 {
		t.Fatalf("unlocated = %v, want [1]", got.Unlocated)
	}
}

func TestActivityRefsPointAtTheOriginalItineraryIndex(t *testing.T) {
	// Index 1 is unlocatable, so the second stop must still report ref 2.
	day := types.DayPlan{Activities: activities("Shinjuku", "nowhere", "Ginza")}
	got := buildDay(day, locatorFor(map[string]Point{"Shinjuku": shinjuku, "Ginza": ginza}))

	refs := map[int]bool{}
	for _, s := range got.Ordered {
		refs[s.ActivityRef] = true
	}
	if !refs[0] || !refs[2] {
		t.Fatalf("refs = %v, want 0 and 2", refs)
	}
}

func TestLongHopsSwitchTheDayToDriving(t *testing.T) {
	walkable := buildDay(
		types.DayPlan{Activities: activities("Gyoen", "Shinjuku")},
		locatorFor(map[string]Point{"Gyoen": gyoen, "Shinjuku": shinjuku}),
	)
	if walkable.Mode != ModeWalk {
		t.Fatalf("nearby stops gave mode %q, want %q", walkable.Mode, ModeWalk)
	}

	far := buildDay(
		types.DayPlan{Activities: activities("Shinjuku", "Haneda")},
		locatorFor(map[string]Point{"Shinjuku": shinjuku, "Haneda": haneda}),
	)
	if far.Mode != ModeDriv {
		t.Fatalf("distant stops gave mode %q, want %q", far.Mode, ModeDriv)
	}
	for _, leg := range far.Legs {
		if leg.Mode != ModeDriv {
			t.Fatalf("leg mode %q, want %q", leg.Mode, ModeDriv)
		}
	}
}

func TestTotalsAreTheSumOfLegs(t *testing.T) {
	day := types.DayPlan{Activities: activities("Shinjuku", "Shibuya", "Ginza")}
	got := buildDay(day, locatorFor(map[string]Point{
		"Shinjuku": shinjuku, "Shibuya": shibuya, "Ginza": ginza,
	}))

	if len(got.Legs) != len(got.Ordered)-1 {
		t.Fatalf("%d legs for %d stops", len(got.Legs), len(got.Ordered))
	}
	var distance, duration float64
	for _, l := range got.Legs {
		distance += l.DistanceM
		duration += l.DurationS
		if l.DurationS <= 0 {
			t.Fatalf("leg %+v has no duration", l)
		}
	}
	if distance != got.TotalDistance || duration != got.TotalDuration {
		t.Fatalf("totals (%.0f, %.0f) do not match leg sums (%.0f, %.0f)",
			got.TotalDistance, got.TotalDuration, distance, duration)
	}
}

func TestDayWithNoLocatableActivitiesIsEmptyNotNil(t *testing.T) {
	// The JSON is rendered directly, and the frontend indexes into these arrays.
	got := buildDay(types.DayPlan{Activities: activities("nowhere")}, locatorFor(nil))

	if got.Ordered == nil || got.Legs == nil || got.Unlocated == nil {
		t.Fatalf("nil slice would serialise as JSON null: %+v", got)
	}
	if len(got.Ordered) != 0 || got.TotalDistance != 0 {
		t.Fatalf("expected an empty route, got %+v", got)
	}
}

func TestSingleStopDayHasNoLegs(t *testing.T) {
	got := buildDay(types.DayPlan{Activities: activities("Shinjuku")}, locatorFor(map[string]Point{"Shinjuku": shinjuku}))
	if len(got.Ordered) != 1 || len(got.Legs) != 0 {
		t.Fatalf("got %d stops and %d legs, want 1 and 0", len(got.Ordered), len(got.Legs))
	}
}

func TestBuildCoversEveryDay(t *testing.T) {
	days := []types.DayPlan{
		{DayIndex: 0, Activities: activities("Shinjuku", "Shibuya")},
		{DayIndex: 1, Activities: activities("nowhere at all")},
	}
	got := Build(days, locatorFor(map[string]Point{"Shinjuku": shinjuku, "Shibuya": shibuya}))

	if len(got.Days) != 2 {
		t.Fatalf("built %d days, want 2", len(got.Days))
	}
	if got.GeneratedAt == "" {
		t.Fatal("GeneratedAt is empty; the frontend polls on it changing")
	}
	if got.Days[1].DayIndex != 1 {
		t.Fatalf("day index %d, want 1", got.Days[1].DayIndex)
	}
}

// --- helpers ---

func names(stops []Stop) []string {
	out := make([]string, len(stops))
	for i, s := range stops {
		out[i] = s.Name
	}
	return out
}

func tourLength(stops []Stop) float64 {
	var total float64
	for i := 0; i+1 < len(stops); i++ {
		total += math.Round(legDistance(stops[i], stops[i+1]))
	}
	return total
}
