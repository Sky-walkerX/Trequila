// Package scoring chooses which real places a trip is planned from.
//
// Rank is a pure, deterministic function: the same places, interests, dates and weather
// always give the same list in the same order, whatever order the places arrive in. Every
// point a place earns or loses comes with a reason in plain words, which the product
// shows as "why this place" and which makes the ranking explainable rather than magic.
package scoring

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/sky-walkerx/trequila/internal/hours"
	"github.com/sky-walkerx/trequila/internal/osm"
	"github.com/sky-walkerx/trequila/internal/popularity"
	"github.com/sky-walkerx/trequila/internal/weather"
)

// How many candidates to keep per trip day. The planner picks a subset for each day, so
// the pool is deliberately larger than a day's plan.
const (
	SightsPerDay = 5
	MealsPerDay  = 3
)

// Point is a coordinate.
type Point struct{ Lat, Lon float64 }

// Candidate is a real place from OpenStreetMap with the facts scoring needs.
type Candidate struct {
	Ref          string // stable OSM reference, e.g. "node/123"
	Name         string
	Kind         osm.Kind
	Category     string
	Lat, Lon     float64
	OpeningHours string // raw OSM opening_hours; empty when unmapped
	Setting      osm.Setting
	Free         bool
	Wikidata     string
	Signals      popularity.Signals
}

// Input is everything a ranking depends on.
type Input struct {
	Places    []Candidate
	Interests []string
	Dates     []time.Time   // the trip's days, local dates
	Days      []weather.Day // optional; matched to Dates by date
	Center    Point         // the destination's centre
}

// Scored is a chosen candidate with its score and the reasons behind it.
type Scored struct {
	Candidate
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
}

// Excluded is a place that was dropped, and why.
type Excluded struct {
	Ref    string `json:"ref"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Result is the ranked, capped selection.
type Result struct {
	Sights   []Scored
	Meals    []Scored
	Excluded []Excluded
}

// Rank scores every place, drops those that are closed for the whole trip, removes
// near-duplicates and keeps the best of each kind under per-category caps.
func Rank(in Input) Result {
	var res Result
	wetDays, totalDays := wetCount(in)
	var sights, meals []Scored

	for _, c := range in.Places {
		if c.Kind != osm.KindSight && c.Kind != osm.KindMeal {
			continue
		}
		s := Scored{Candidate: c}
		dropped := scoreHours(&s, in.Dates)
		if dropped {
			res.Excluded = append(res.Excluded, Excluded{Ref: c.Ref, Name: c.Name, Reason: "closed on every day of the trip"})
			continue
		}
		scoreInterest(&s, in.Interests)
		scorePopularity(&s)
		scoreDistance(&s, in.Center)
		scoreWeather(&s, wetDays, totalDays)
		if c.Free {
			add(&s, 0.2, "free entry")
		}
		if len(s.Reasons) == 0 {
			s.Reasons = []string{"a real place in OpenStreetMap near the centre"}
		}
		s.Score = math.Round(s.Score*10000) / 10000
		if c.Kind == osm.KindMeal {
			meals = append(meals, s)
		} else {
			sights = append(sights, s)
		}
	}

	days := max(len(in.Dates), 1)
	res.Sights = pick(sights, days*SightsPerDay, 0.4)
	res.Meals = pick(meals, days*MealsPerDay, 0.5)
	sort.Slice(res.Excluded, func(i, j int) bool { return res.Excluded[i].Ref < res.Excluded[j].Ref })
	return res
}

func add(s *Scored, points float64, reason string) {
	s.Score += points
	if reason != "" {
		s.Reasons = append(s.Reasons, reason)
	}
}

// scoreHours checks the place against each trip date and reports whether it is closed
// on all of them.
func scoreHours(s *Scored, dates []time.Time) (closedAllTrip bool) {
	if s.OpeningHours == "" || len(dates) == 0 {
		add(s, -0.5, "opening hours unknown")
		return false
	}
	sched, err := hours.Parse(s.OpeningHours)
	if err != nil {
		add(s, -0.5, "opening hours unknown")
		return false
	}
	open := 0
	for _, d := range dates {
		if sched.OpenOn(d) {
			open++
		}
	}
	switch {
	case open == 0:
		return true
	case open == len(dates):
		add(s, 0.5, fmt.Sprintf("open on all %d trip days", len(dates)))
	default:
		add(s, 0.2, fmt.Sprintf("open on %d of %d trip days", open, len(dates)))
	}
	return false
}

// interestCategories maps a theme a traveller might name to the OSM categories that serve it.
var interestCategories = map[string][]string{
	"food":      {"restaurant", "cafe", "marketplace", "fast_food"},
	"history":   {"historic", "castle", "monument", "ruins", "memorial", "museum", "fort", "archaeological_site"},
	"art":       {"gallery", "museum", "artwork", "arts_centre", "theatre"},
	"museum":    {"museum", "gallery"},
	"culture":   {"museum", "theatre", "arts_centre", "gallery", "historic", "castle"},
	"nature":    {"park", "garden", "nature_reserve", "beach", "peak", "viewpoint"},
	"outdoor":   {"park", "garden", "nature_reserve", "beach", "peak", "viewpoint"},
	"hik":       {"nature_reserve", "peak", "viewpoint"},
	"beach":     {"beach"},
	"view":      {"viewpoint", "peak"},
	"nightlife": {"bar", "pub", "theatre"},
	"bar":       {"bar", "pub"},
	"shopping":  {"marketplace"},
	"market":    {"marketplace"},
	"family":    {"zoo", "theme_park", "aquarium", "park"},
	"kid":       {"zoo", "theme_park", "aquarium", "park"},
	"architect": {"historic", "castle", "attraction", "monument"},
}

func scoreInterest(s *Scored, interests []string) {
	best := ""
	for _, in := range interests {
		in = strings.ToLower(strings.TrimSpace(in))
		if in == "" {
			continue
		}
		matched := false
		for key, cats := range interestCategories {
			if strings.Contains(in, key) && contains(cats, s.Category) {
				matched = true
			}
		}
		if !matched && len(in) >= 4 && strings.Contains(fold(s.Name), fold(in)) {
			matched = true
		}
		if matched && (best == "" || in < best) { // the alphabetically first keeps the reason stable
			best = in
		}
	}
	if best != "" {
		add(s, 2.0, "matches your interest in "+best)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func scorePopularity(s *Scored) {
	sl := math.Min(1, math.Log1p(float64(s.Signals.Sitelinks))/math.Log1p(200))
	pv := math.Min(1, math.Log1p(float64(s.Signals.Pageviews))/math.Log1p(2e6))
	weight := 1.0
	if s.Kind == osm.KindMeal {
		weight = 0.4 // landmarks matter more than restaurants with a Wikipedia article
	}
	s.Score += weight * (3*sl + 2*pv)
	if s.Signals.Sitelinks >= 5 {
		s.Reasons = append(s.Reasons, fmt.Sprintf("covered in %d Wikipedia languages", s.Signals.Sitelinks))
	}
	if s.Signals.Pageviews >= 10000 {
		s.Reasons = append(s.Reasons, fmt.Sprintf("%s English Wikipedia views a year", compact(s.Signals.Pageviews)))
	}
}

func compact(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}

func scoreDistance(s *Scored, center Point) {
	d := distance(s.Lat, s.Lon, center.Lat, center.Lon)
	add(s, 1.5*math.Exp(-d/3000), "")
	if d < 1000 {
		s.Reasons = append(s.Reasons, "within 1 km of the centre")
	}
}

func wetCount(in Input) (wet, total int) {
	total = len(in.Dates)
	if total == 0 || len(in.Days) == 0 {
		return 0, total
	}
	byDate := map[string]weather.Day{}
	for _, d := range in.Days {
		byDate[d.Date] = d
	}
	for _, d := range in.Dates {
		if w, ok := byDate[d.Format("2006-01-02")]; ok && w.Wet() {
			wet++
		}
	}
	return wet, total
}

func scoreWeather(s *Scored, wet, total int) {
	if wet == 0 || total == 0 {
		return
	}
	frac := float64(wet) / float64(total)
	switch s.Setting {
	case osm.SettingIndoor:
		add(s, 1.0*frac, fmt.Sprintf("indoor, and rain is expected on %d of %d days", wet, total))
	case osm.SettingOutdoor:
		add(s, -0.5*frac, "")
	}
}

// pick returns the n best by score under a per-category cap (a fraction of n, never below
// three) and with near-duplicates removed. Sorting first, with the reference as the final
// tiebreak, is what makes the outcome independent of input order.
func pick(all []Scored, n int, capFrac float64) []Scored {
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].Ref < all[j].Ref
	})
	limit := max(3, int(math.Ceil(capFrac*float64(n))))
	perCategory := map[string]int{}
	var out []Scored
	for _, c := range all {
		if len(out) == n {
			break
		}
		if perCategory[c.Category] >= limit || duplicate(out, c) {
			continue
		}
		perCategory[c.Category]++
		out = append(out, c)
	}
	return out
}

// duplicate reports whether an already chosen place has the same name within 150 m.
func duplicate(chosen []Scored, c Scored) bool {
	name := fold(c.Name)
	for _, o := range chosen {
		if fold(o.Name) == name && distance(o.Lat, o.Lon, c.Lat, c.Lon) < 150 {
			return true
		}
	}
	return false
}

var accents = strings.NewReplacer(
	"à", "a", "á", "a", "â", "a", "ã", "a", "ä", "a", "å", "a", "ā", "a",
	"ç", "c", "ć", "c", "č", "c",
	"è", "e", "é", "e", "ê", "e", "ë", "e", "ē", "e", "ě", "e",
	"ì", "i", "í", "i", "î", "i", "ï", "i", "ī", "i",
	"ñ", "n", "ń", "n", "ň", "n",
	"ò", "o", "ó", "o", "ô", "o", "õ", "o", "ö", "o", "ø", "o", "ō", "o", "ő", "o",
	"ù", "u", "ú", "u", "û", "u", "ü", "u", "ū", "u", "ů", "u", "ű", "u",
	"ý", "y", "ÿ", "y", "ß", "ss", "š", "s", "ś", "s", "ž", "z", "ź", "z", "ł", "l",
)

// fold lowercases, strips common accents and drops everything but letters and digits, so
// "Café  Central" and "cafe central" compare equal.
func fold(s string) string {
	s = accents.Replace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func distance(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}
