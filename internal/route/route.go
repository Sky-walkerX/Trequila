// Package route turns a day's list of activities into a visiting order.
//
// Distances are straight-line (haversine) and durations are derived from a
// walking or driving speed, so a leg is an estimate of effort rather than a
// street-accurate figure. That is deliberate: ordering a handful of stops well
// only needs relative distances, and keeping the computation local avoids
// putting a routing provider's rate limit and uptime on the trip-planning path.
package route

import (
	"math"
	"sort"
	"time"

	"github.com/sky-walkerx/trequila/internal/types"
)

// Travel modes, and the speeds used to turn a distance into a duration. Both
// are deliberately conservative city averages: walking includes crossings and
// waiting, driving includes traffic and parking hunting.
const (
	ModeWalk = "walk"
	ModeDriv = "drive"

	walkSpeedMPS  = 1.3 // ~4.7 km/h
	driveSpeedMPS = 7.0 // ~25 km/h

	// A day is walkable when no single hop exceeds this. Beyond it the day is
	// routed as driving, which is also what the map layer renders.
	walkableLegLimitM = 2000.0

	// Straight-line distance underestimates real travel, because streets do not
	// run diagonally. This scales legs up to something closer to street distance.
	detourFactor = 1.25

	earthRadiusM = 6371000.0
)

// Stop is one activity placed on the map. Field names match the frontend's
// RouteStop exactly — this JSON is rendered directly.
type Stop struct {
	ActivityRef int     `json:"activityRef"` // index into the day's activities
	Name        string  `json:"name"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Time        string  `json:"time"`
	Geocoded    bool    `json:"geocoded"` // false when the coordinates came from the trip's places
}

// Leg is the hop between two consecutive stops.
type Leg struct {
	FromRef   int     `json:"fromRef"`
	ToRef     int     `json:"toRef"`
	DistanceM float64 `json:"distance_m"`
	DurationS float64 `json:"duration_s"`
	Mode      string  `json:"mode"`
}

// Day is one day's ordered route.
type Day struct {
	DayIndex      int     `json:"dayIndex"`
	Mode          string  `json:"mode"`
	Ordered       []Stop  `json:"ordered"`
	Legs          []Leg   `json:"legs"`
	Unlocated     []int   `json:"unlocated"` // activity refs with no known coordinates
	TotalDistance float64 `json:"total_distance_m"`
	TotalDuration float64 `json:"total_duration_s"`
}

// Data is the whole trip's route, stored as the trips.route JSONB column.
type Data struct {
	GeneratedAt string `json:"generated_at"`
	Days        []Day  `json:"days"`
}

// Point is a resolved coordinate for one activity.
type Point struct {
	Lat      float64
	Lon      float64
	Geocoded bool
}

// Locator resolves an activity to coordinates, reporting ok=false when it
// cannot. Implementations are expected to be cached and rate-limited.
type Locator interface {
	Locate(activity types.Activity) (Point, bool)
}

// LocatorFunc adapts a plain function to Locator.
type LocatorFunc func(types.Activity) (Point, bool)

func (f LocatorFunc) Locate(a types.Activity) (Point, bool) { return f(a) }

// Build orders every day of the itinerary. Days whose activities cannot be
// placed still appear, carrying their unlocated refs, so the caller can tell
// "nothing to route" apart from "not computed yet".
func Build(days []types.DayPlan, locator Locator) Data {
	out := Data{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Days:        make([]Day, 0, len(days)),
	}
	for _, day := range days {
		out.Days = append(out.Days, buildDay(day, locator))
	}
	return out
}

func buildDay(day types.DayPlan, locator Locator) Day {
	result := Day{
		DayIndex:  day.DayIndex,
		Mode:      ModeWalk,
		Ordered:   []Stop{},
		Legs:      []Leg{},
		Unlocated: []int{},
	}

	var stops []Stop
	for i, activity := range day.Activities {
		point, ok := locator.Locate(activity)
		if !ok {
			result.Unlocated = append(result.Unlocated, i)
			continue
		}
		stops = append(stops, Stop{
			ActivityRef: i,
			Name:        activity.Name,
			Lat:         point.Lat,
			Lon:         point.Lon,
			Time:        activity.Time,
			Geocoded:    point.Geocoded,
		})
	}

	if len(stops) == 0 {
		return result
	}

	ordered := order(stops)
	result.Mode = modeFor(ordered)
	result.Ordered = ordered

	speed := walkSpeedMPS
	if result.Mode == ModeDriv {
		speed = driveSpeedMPS
	}
	for i := 0; i+1 < len(ordered); i++ {
		d := legDistance(ordered[i], ordered[i+1])
		leg := Leg{
			FromRef:   ordered[i].ActivityRef,
			ToRef:     ordered[i+1].ActivityRef,
			DistanceM: math.Round(d),
			DurationS: math.Round(d / speed),
			Mode:      result.Mode,
		}
		result.Legs = append(result.Legs, leg)
		result.TotalDistance += leg.DistanceM
		result.TotalDuration += leg.DurationS
	}
	return result
}

// order picks a visiting sequence. Activities that carry a time of day are
// already an intended order and are left alone — a route that ignores a dinner
// reservation is worse than a slightly longer walk. Only when a day has no
// usable times does it get reordered geographically.
func order(stops []Stop) []Stop {
	if timed := sortByTime(stops); timed != nil {
		return timed
	}
	return twoOpt(nearestNeighbour(stops))
}

// sortByTime returns stops in clock order, or nil when any time is missing or
// unparseable — a partial ordering would be arbitrary for the rest.
func sortByTime(stops []Stop) []Stop {
	type timed struct {
		stop   Stop
		minute int
	}
	entries := make([]timed, 0, len(stops))
	for _, s := range stops {
		m, ok := parseClock(s.Time)
		if !ok {
			return nil
		}
		entries = append(entries, timed{stop: s, minute: m})
	}
	// Stable so activities sharing a time keep their itinerary order.
	sort.SliceStable(entries, func(a, b int) bool { return entries[a].minute < entries[b].minute })

	out := make([]Stop, len(entries))
	for i, e := range entries {
		out[i] = e.stop
	}
	return out
}

// parseClock reads the handful of time shapes the LLM emits: "09:00", "9:00 AM",
// "14:30", "7 PM". Anything else is treated as absent.
func parseClock(value string) (int, bool) {
	for _, layout := range []string{"15:04", "3:04 PM", "3:04PM", "3 PM", "3PM", "15:04:05"} {
		if t, err := time.Parse(layout, normaliseClock(value)); err == nil {
			return t.Hour()*60 + t.Minute(), true
		}
	}
	return 0, false
}

func normaliseClock(value string) string {
	out := make([]rune, 0, len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, r-'a'+'A')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// nearestNeighbour walks from the first stop, always hopping to the closest
// unvisited one. Cheap, and a good starting tour for twoOpt to improve.
func nearestNeighbour(stops []Stop) []Stop {
	remaining := append([]Stop(nil), stops...)
	tour := []Stop{remaining[0]}
	remaining = remaining[1:]

	for len(remaining) > 0 {
		current := tour[len(tour)-1]
		best, bestDist := 0, math.MaxFloat64
		for i, candidate := range remaining {
			if d := legDistance(current, candidate); d < bestDist {
				best, bestDist = i, d
			}
		}
		tour = append(tour, remaining[best])
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	return tour
}

// twoOpt repeatedly reverses a segment whenever doing so shortens the tour.
// A day holds a handful of stops, so the O(n²) sweep is trivial and runs to a
// local optimum rather than being cut off by an iteration budget.
func twoOpt(tour []Stop) []Stop {
	if len(tour) < 4 {
		return tour
	}
	best := append([]Stop(nil), tour...)
	for improved := true; improved; {
		improved = false
		for i := 1; i < len(best)-1; i++ {
			for j := i + 1; j < len(best); j++ {
				if delta(best, i, j) < -0.5 { // metres; ignore floating-point noise
					reverse(best[i : j+1])
					improved = true
				}
			}
		}
	}
	return best
}

// delta is the change in tour length from reversing best[i:j+1].
func delta(tour []Stop, i, j int) float64 {
	before := legDistance(tour[i-1], tour[i])
	after := legDistance(tour[i-1], tour[j])
	if j+1 < len(tour) {
		before += legDistance(tour[j], tour[j+1])
		after += legDistance(tour[i], tour[j+1])
	}
	return after - before
}

func reverse(s []Stop) {
	for a, b := 0, len(s)-1; a < b; a, b = a+1, b-1 {
		s[a], s[b] = s[b], s[a]
	}
}

// modeFor picks walking unless some hop is too far to walk.
func modeFor(stops []Stop) string {
	for i := 0; i+1 < len(stops); i++ {
		if legDistance(stops[i], stops[i+1]) > walkableLegLimitM {
			return ModeDriv
		}
	}
	return ModeWalk
}

// legDistance is the haversine distance scaled towards street distance.
func legDistance(a, b Stop) float64 {
	return Haversine(a.Lat, a.Lon, b.Lat, b.Lon) * detourFactor
}

// Haversine returns the great-circle distance between two points, in metres.
func Haversine(lat1, lon1, lat2, lon2 float64) float64 {
	φ1, φ2 := lat1*math.Pi/180, lat2*math.Pi/180
	Δφ := (lat2 - lat1) * math.Pi / 180
	Δλ := (lon2 - lon1) * math.Pi / 180

	h := math.Sin(Δφ/2)*math.Sin(Δφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(Δλ/2)*math.Sin(Δλ/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(h)))
}
