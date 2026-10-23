// Package schedule turns a day's places into a timed route. It orders the sights to keep
// travel short, simulates the day from 09:00 in the destination's local time, fits meals
// into lunch and dinner, and checks every visit against the place's opening hours: a
// place is moved to a time it is open when that is possible, and otherwise flagged as a
// conflict. Places whose hours are unknown are never treated as closed.
package schedule

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/sky-walkerx/trequila/internal/hours"
	"github.com/sky-walkerx/trequila/internal/ors"
)

// Provider supplies travel times; *ors.Client satisfies it.
type Provider interface {
	Matrix(ctx context.Context, profile ors.Profile, pts []ors.Point) (ors.Matrix, error)
}

// Stop is one place to visit.
type Stop struct {
	ItemID       string
	PlaceID      string
	Name         string
	Kind         string // "sight" | "meal"
	Category     string
	Slot         string // meals: "lunch" | "dinner"
	Lat, Lon     float64
	OpeningHours string
}

// Params fixes the day. Date must be local midnight in the destination's zone.
type Params struct {
	Date time.Time
}

// Hours states.
const (
	HoursOpen    = "open"    // the visit fits inside the opening hours
	HoursUnknown = "unknown" // no usable hours: assumed fine, never a conflict
	HoursClosed  = "closed"  // the visit is outside the hours (see Conflicts)
)

// Conflict kinds.
const (
	ConflictClosedToday     = "closed_today"
	ConflictClosedAtArrival = "closed_at_arrival"
	ConflictClosesBeforeEnd = "closes_before_end"
)

// Visit is a stop with its times.
type Visit struct {
	ItemID       string    `json:"item_id"`
	PlaceID      string    `json:"place_id"`
	Order        int       `json:"order"`
	Arrive       time.Time `json:"arrive"`
	Depart       time.Time `json:"depart"`
	WaitS        int       `json:"wait_s"`         // time spent waiting for the place to open
	LegDurationS int       `json:"leg_duration_s"` // travel from the previous stop
	LegDistanceM int       `json:"leg_distance_m"`
	Hours        string    `json:"hours"`
}

// Conflict is a visit that cannot be made to fit the opening hours.
type Conflict struct {
	ItemID  string `json:"item_id"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Day is the scheduled result.
type Day struct {
	Visits         []Visit    `json:"visits"`
	Conflicts      []Conflict `json:"conflicts"`
	Mode           string     `json:"mode"` // "walk" | "drive"
	Estimated      bool       `json:"estimated"`
	TotalDistanceM int        `json:"total_distance_m"`
	TotalTravelS   int        `json:"total_travel_s"`
}

const (
	dayStartMin       = 9 * 60
	lunchFromMin      = 11*60 + 30
	lunchClockMin     = 12 * 60
	dinnerFromMin     = 18*60 + 30
	lateAfterMin      = 22 * 60
	driveAboveM       = 2000.0
	minUsefulVisit    = 20 * time.Minute
	conflictPenalty   = 1e6
	maxImprovementRun = 200
)

var visitLength = map[string]time.Duration{
	"museum": 90 * time.Minute, "gallery": 75 * time.Minute, "castle": 90 * time.Minute, "aquarium": 120 * time.Minute,
	"zoo": 150 * time.Minute, "theme_park": 180 * time.Minute, "park": 60 * time.Minute, "garden": 60 * time.Minute,
	"beach": 90 * time.Minute, "viewpoint": 20 * time.Minute, "peak": 60 * time.Minute, "artwork": 15 * time.Minute,
	"theatre": 120 * time.Minute, "marketplace": 60 * time.Minute,
}

func duration(s Stop) time.Duration {
	if s.Kind == "meal" {
		if s.Slot == "dinner" {
			return 90 * time.Minute
		}
		return 60 * time.Minute
	}
	if d, ok := visitLength[s.Category]; ok {
		return d
	}
	return 60 * time.Minute
}

// PlanDay schedules one day.
func PlanDay(ctx context.Context, prov Provider, stops []Stop, p Params) (Day, error) {
	if len(stops) == 0 {
		return Day{Visits: []Visit{}, Conflicts: []Conflict{}, Mode: "walk"}, nil
	}
	// Sights first, meals after; indexes into this slice are used throughout.
	var sights, lunches, dinners []int
	var ordered []Stop
	for _, s := range stops {
		if s.Kind != "meal" {
			ordered = append(ordered, s)
		}
	}
	for _, s := range stops {
		if s.Kind == "meal" {
			ordered = append(ordered, s)
		}
	}
	stops = ordered
	for i, s := range stops {
		switch {
		case s.Kind != "meal":
			sights = append(sights, i)
		case s.Slot == "dinner":
			dinners = append(dinners, i)
		default:
			lunches = append(lunches, i)
		}
	}

	pts := make([]ors.Point, len(stops))
	for i, s := range stops {
		pts[i] = ors.Point{Lat: s.Lat, Lon: s.Lon}
	}

	// Decide the mode from straight-line estimates, then ask for real times in that mode.
	profile, mode := ors.Walking, "walk"
	if longestNearestLeg(ors.Estimate(ors.Walking, pts), sights) > driveAboveM {
		profile, mode = ors.Driving, "drive"
	}
	mat, err := prov.Matrix(ctx, profile, pts)
	if err != nil {
		return Day{}, fmt.Errorf("schedule: travel times: %w", err)
	}

	sim := &simulator{stops: stops, mat: mat, date: p.Date, schedules: parseHours(stops)}
	sim.lunches, sim.dinners = lunches, dinners

	order := nearestNeighbour(mat, sights)
	best := sim.run(order)
	for pass := 0; pass < maxImprovementRun; pass++ {
		improved := false
		for _, cand := range neighbours(order) {
			if r := sim.run(cand); r.cost < best.cost-1e-9 {
				order, best, improved = cand, r, true
				break // first improvement; restart from the new order
			}
		}
		if !improved {
			break
		}
	}

	day := Day{Visits: best.visits, Conflicts: best.conflicts, Mode: mode, Estimated: mat.Estimated}
	if day.Conflicts == nil {
		day.Conflicts = []Conflict{}
	}
	for _, v := range day.Visits {
		day.TotalDistanceM += v.LegDistanceM
		day.TotalTravelS += v.LegDurationS
	}
	return day, nil
}

// longestNearestLeg is the longest hop of a nearest-neighbour walk through the sights.
func longestNearestLeg(m ors.Matrix, sights []int) float64 {
	if len(sights) < 2 {
		return 0
	}
	order := nearestNeighbour(m, sights)
	longest := 0.0
	for i := 1; i < len(order); i++ {
		longest = math.Max(longest, m.Distances[order[i-1]][order[i]])
	}
	return longest
}

// nearestNeighbour orders idx greedily by travel time, starting from idx[0] (the best
// ranked sight). Ties go to the earlier index, so the result is deterministic.
func nearestNeighbour(m ors.Matrix, idx []int) []int {
	if len(idx) == 0 {
		return nil
	}
	out := []int{idx[0]}
	left := append([]int(nil), idx[1:]...)
	for len(left) > 0 {
		last := out[len(out)-1]
		bi := 0
		for i := range left {
			if m.Durations[last][left[i]] < m.Durations[last][left[bi]] {
				bi = i
			}
		}
		out = append(out, left[bi])
		left = append(left[:bi], left[bi+1:]...)
	}
	return out
}

// neighbours lists the orders one move away: every reversal of a segment and every
// single-stop relocation, in a fixed order.
func neighbours(order []int) [][]int {
	n := len(order)
	var out [][]int
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			c := append([]int(nil), order...)
			for a, b := i, j; a < b; a, b = a+1, b-1 {
				c[a], c[b] = c[b], c[a]
			}
			out = append(out, c)
		}
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			c := append([]int(nil), order[:i]...)
			c = append(c, order[i+1:]...)
			c = append(c[:j], append([]int{order[i]}, c[j:]...)...)
			out = append(out, c)
		}
	}
	return out
}

func parseHours(stops []Stop) []*hours.Schedule {
	out := make([]*hours.Schedule, len(stops))
	for i, s := range stops {
		if s.OpeningHours == "" {
			continue
		}
		if sc, err := hours.Parse(s.OpeningHours); err == nil {
			out[i] = &sc
		}
	}
	return out
}

type result struct {
	visits    []Visit
	conflicts []Conflict
	cost      float64
}

type simulator struct {
	stops     []Stop
	mat       ors.Matrix
	date      time.Time
	schedules []*hours.Schedule
	lunches   []int
	dinners   []int
}

func (s *simulator) at(minutes int) time.Time {
	y, m, d := s.date.Date()
	return time.Date(y, m, d, 0, minutes, 0, 0, s.date.Location())
}

// run walks the day for one ordering of the sights, placing lunch once the clock reaches
// midday and dinner at the end.
func (s *simulator) run(order []int) result {
	var r result
	clock := s.at(dayStartMin)
	prev := -1
	var travel, wait float64

	visit := func(i int) {
		var legS, legM float64
		if prev >= 0 {
			legS, legM = s.mat.Durations[prev][i], s.mat.Distances[prev][i]
		}
		arrive := clock.Add(time.Duration(legS) * time.Second)
		stop := s.stops[i]
		if stop.Kind == "meal" {
			from := lunchFromMin
			if stop.Slot == "dinner" {
				from = dinnerFromMin
			}
			if open := s.at(from); arrive.Before(open) {
				arrive = open
			}
		}
		start, state, conflict := s.fit(i, arrive, duration(stop))
		if conflict != nil {
			r.conflicts = append(r.conflicts, *conflict)
			r.cost += conflictPenalty
		}
		end := start.Add(duration(stop))
		if state == HoursClosed && conflict != nil && conflict.Kind == ConflictClosesBeforeEnd {
			if closes, ok := s.closing(i, start); ok && closes.Before(end) {
				end = closes
			}
		}
		w := start.Sub(clock.Add(time.Duration(legS) * time.Second))
		if w < 0 {
			w = 0
		}
		r.visits = append(r.visits, Visit{
			ItemID: stop.ItemID, PlaceID: stop.PlaceID, Order: len(r.visits),
			Arrive: start, Depart: end, WaitS: int(w.Seconds()),
			LegDurationS: int(legS), LegDistanceM: int(legM), Hours: state,
		})
		travel += legS
		wait += w.Seconds()
		clock, prev = end, i
	}

	lunch := append([]int(nil), s.lunches...)
	for _, i := range order {
		for len(lunch) > 0 && !clock.Before(s.at(lunchClockMin)) {
			visit(lunch[0])
			lunch = lunch[1:]
		}
		visit(i)
	}
	for _, i := range lunch {
		visit(i)
	}
	for _, i := range s.dinners {
		visit(i)
	}

	r.cost += travel + wait*0.5
	if late := clock.Sub(s.at(lateAfterMin)).Seconds(); late > 0 {
		r.cost += late * 2
	}
	return r
}

// window is an open interval on the day.
type window struct{ start, end time.Time }

func (s *simulator) windows(i int) ([]window, bool) {
	sc := s.schedules[i]
	if sc == nil {
		return nil, false
	}
	ws := sc.Windows(s.date)
	out := make([]window, len(ws))
	for k, w := range ws {
		out[k] = window{w.Start, w.End}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].start.Before(out[b].start) })
	return out, true
}

func (s *simulator) closing(i int, at time.Time) (time.Time, bool) {
	ws, _ := s.windows(i)
	for _, w := range ws {
		if !at.Before(w.start) && at.Before(w.end) {
			return w.end, true
		}
	}
	return time.Time{}, false
}

// fit finds when a visit that could begin at arrive actually happens, given the hours.
func (s *simulator) fit(i int, arrive time.Time, dur time.Duration) (time.Time, string, *Conflict) {
	ws, known := s.windows(i)
	if !known {
		return arrive, HoursUnknown, nil
	}
	stop := s.stops[i]
	if len(ws) == 0 {
		return arrive, HoursClosed, &Conflict{ItemID: stop.ItemID, Kind: ConflictClosedToday,
			Message: fmt.Sprintf("%s is closed on %s", stop.Name, s.date.Weekday())}
	}
	for _, w := range ws {
		if !arrive.Before(w.end) {
			continue // this window is over
		}
		start := arrive
		if start.Before(w.start) {
			start = w.start // wait for it to open
		}
		remaining := w.end.Sub(start)
		switch {
		case remaining >= dur:
			return start, HoursOpen, nil
		case remaining >= minUsefulVisit:
			return start, HoursClosed, &Conflict{ItemID: stop.ItemID, Kind: ConflictClosesBeforeEnd,
				Message: fmt.Sprintf("%s closes at %s, before a normal visit would end", stop.Name, w.end.Format("15:04"))}
		}
		// Too little time left in this window: try the next one.
	}
	return arrive, HoursClosed, &Conflict{ItemID: stop.ItemID, Kind: ConflictClosedAtArrival,
		Message: fmt.Sprintf("%s is closed by the time we get there (%s)", stop.Name, arrive.Format("15:04"))}
}
