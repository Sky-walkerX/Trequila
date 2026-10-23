// Package grounding keeps an itinerary honest. The language model is only asked to
// arrange a fixed list of real places by ID; what it returns is validated against that
// list, repaired once by asking again, and finally normalised deterministically, so the
// stored plan never contains a place that was not on the list, whatever the model does.
package grounding

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Kinds and slots.
const (
	KindSight = "sight"
	KindMeal  = "meal"

	SlotMorning   = "morning"
	SlotAfternoon = "afternoon"
	SlotEvening   = "evening"
	SlotLunch     = "lunch"
	SlotDinner    = "dinner"
)

// Candidate is a real place the plan may use. Lists are best-first.
type Candidate struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Category     string   `json:"category"`
	Lat          float64  `json:"lat"`
	Lon          float64  `json:"lon"`
	OpeningHours string   `json:"opening_hours,omitempty"`
	Setting      string   `json:"setting,omitempty"`
	Reasons      []string `json:"reasons,omitempty"`
}

// Item is one place in a day.
type Item struct {
	ItemID  string `json:"item_id,omitempty"` // stable, assigned by Finalise
	PlaceID string `json:"place_id"`
	Slot    string `json:"slot,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Day is one day of a plan.
type Day struct {
	DayID string `json:"day_id,omitempty"` // stable, assigned by Finalise
	Index int    `json:"index"`
	Date  string `json:"date,omitempty"`
	Title string `json:"title"`
	Items []Item `json:"items"`
}

// Plan is an itinerary made only of candidate IDs.
type Plan struct {
	Days []Day `json:"days"`
}

// Problem is one way a plan breaks the rules, worded for the model that has to fix it.
type Problem struct {
	Day     int // 1-based; 0 for the plan as a whole
	Message string
}

func (p Problem) String() string {
	if p.Day == 0 {
		return p.Message
	}
	return fmt.Sprintf("day %d: %s", p.Day, p.Message)
}

// Targets are how many places a day should hold.
type Targets struct{ Sights, Meals int }

const (
	maxSightsPerDay = 5
	maxMealsPerDay  = 2
)

// TargetsFor spreads the available places over the days: up to five sights and two meals
// a day, fewer when the destination offers less.
func TargetsFor(cands []Candidate, days int) Targets {
	var sights, meals int
	for _, c := range cands {
		if c.Kind == KindMeal {
			meals++
		} else {
			sights++
		}
	}
	days = max(days, 1)
	return Targets{
		Sights: min(maxSightsPerDay, max(1, sights/days)),
		Meals:  min(maxMealsPerDay, (meals+days-1)/days),
	}
}

// --- Prompt ---

// PromptInput is what the model needs to arrange a trip.
type PromptInput struct {
	Destination string
	Days        int
	Interests   []string
	Candidates  []Candidate
	WetDays     []int // zero-based indexes of days with rain in the forecast
}

// Prompt asks the model to arrange the candidates, and only the candidates.
func Prompt(in PromptInput) string {
	t := TargetsFor(in.Candidates, in.Days)
	var b strings.Builder
	fmt.Fprintf(&b, "You are planning a %d days trip to %s. Arrange the places below into a day-by-day itinerary.\n\n", in.Days, in.Destination)
	b.WriteString("RULES\n")
	b.WriteString("- Use ONLY the places in the table, referred to by their id in the field place_id. Never invent a place or an id.\n")
	b.WriteString("- Use each place at most once in the whole trip.\n")
	fmt.Fprintf(&b, "- Give every day %d to %d places of kind sight and up to %d of kind meal.\n", max(1, t.Sights-1), t.Sights+1, t.Meals)
	b.WriteString("- A meal uses slot lunch or dinner. A sight uses slot morning, afternoon or evening.\n")
	b.WriteString("- Keep each day's places close together, and respect the opening hours shown.\n")
	if len(in.Interests) > 0 {
		fmt.Fprintf(&b, "- The traveller likes: %s.\n", strings.Join(in.Interests, ", "))
	}
	for _, d := range in.WetDays {
		fmt.Fprintf(&b, "- Rain is forecast on day %d: prefer indoor places for it.\n", d+1)
	}
	b.WriteString("\nPLACES (id | name | kind | category | setting | opening hours | why it was chosen)\n")
	for _, c := range in.Candidates {
		hours := c.OpeningHours
		if hours == "" {
			hours = "unknown"
		}
		setting := c.Setting
		if setting == "" {
			setting = "unknown"
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | %s | %s\n", c.ID, oneLine(c.Name), c.Kind, c.Category, setting, hours, oneLine(strings.Join(c.Reasons, "; ")))
	}
	b.WriteString("\nANSWER with JSON only, in exactly this shape:\n")
	b.WriteString(`{"days":[{"title":"short theme for the day","items":[{"place_id":"<id from the table>","slot":"morning","note":"one practical tip"}]}]}`)
	fmt.Fprintf(&b, "\nThe array days must have exactly %d entries, in order.", in.Days)
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// RepairPrompt asks the model to fix a plan, naming what is wrong with it.
func RepairPrompt(p Plan, problems []Problem) string {
	raw, _ := json.Marshal(p)
	var b strings.Builder
	b.WriteString("Your previous itinerary broke these rules:\n")
	for _, pr := range problems {
		fmt.Fprintf(&b, "- %s\n", pr)
	}
	b.WriteString("\nHere is the itinerary you gave:\n")
	b.Write(raw)
	b.WriteString("\n\nFix it. Use ONLY place_id values from the table in the original request, each at most once. ")
	b.WriteString(`Answer with the corrected JSON only, in the same shape: {"days":[{"title":"...","items":[{"place_id":"...","slot":"...","note":"..."}]}]}`)
	return b.String()
}

// ParsePlan reads the model's answer, tolerating markdown fences and prose around it.
func ParsePlan(raw string) (Plan, error) {
	s := strings.TrimSpace(raw)
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return Plan{}, errors.New("grounding: no JSON object in the answer")
	}
	var p Plan
	if err := json.Unmarshal([]byte(s[start:end+1]), &p); err != nil {
		return Plan{}, fmt.Errorf("grounding: %w", err)
	}
	if len(p.Days) == 0 {
		return Plan{}, errors.New("grounding: the plan has no days")
	}
	return p, nil
}

// --- Validation ---

func isMealSlot(s string) bool { return s == SlotLunch || s == SlotDinner }

// Validate lists every way p breaks the rules. An empty result means the plan is usable.
func Validate(p Plan, cands []Candidate, days int) []Problem {
	byID := make(map[string]Candidate, len(cands))
	for _, c := range cands {
		byID[c.ID] = c
	}
	t := TargetsFor(cands, days)
	var probs []Problem
	if len(p.Days) != days {
		probs = append(probs, Problem{Message: fmt.Sprintf("the plan has %d days but the trip is %d days", len(p.Days), days)})
	}
	usedOn := map[string]int{}
	for i, d := range p.Days {
		n := i + 1
		sights, meals := 0, 0
		for _, it := range d.Items {
			c, ok := byID[it.PlaceID]
			if !ok {
				probs = append(probs, Problem{n, fmt.Sprintf("%q is not one of the candidates", it.PlaceID)})
				continue
			}
			if prev, dup := usedOn[it.PlaceID]; dup {
				probs = append(probs, Problem{n, fmt.Sprintf("%q is already used on day %d", it.PlaceID, prev)})
				continue
			}
			usedOn[it.PlaceID] = n
			if c.Kind == KindMeal {
				meals++
				if !isMealSlot(it.Slot) {
					probs = append(probs, Problem{n, fmt.Sprintf("%q is a meal and must use the lunch or dinner slot", it.PlaceID)})
				}
			} else {
				sights++
				if isMealSlot(it.Slot) {
					probs = append(probs, Problem{n, fmt.Sprintf("%q is a sight and cannot use a meal slot", it.PlaceID)})
				}
			}
		}
		if lo := max(1, t.Sights-1); sights < lo {
			probs = append(probs, Problem{n, fmt.Sprintf("has %d sights, needs at least %d", sights, lo)})
		}
		if hi := t.Sights + 1; sights > hi {
			probs = append(probs, Problem{n, fmt.Sprintf("has %d sights, allows at most %d", sights, hi)})
		}
		if meals > t.Meals {
			probs = append(probs, Problem{n, fmt.Sprintf("has %d meals, allows at most %d", meals, t.Meals)})
		}
	}
	return probs
}

// --- Normalisation ---

// Normalize turns any plan, including an empty one, into a valid plan: it keeps the
// model's valid choices, drops the rest, and fills each day from the unused candidates,
// anchoring a day on its best remaining sight and adding the sights and meals nearest
// to it. It is deterministic.
func Normalize(p Plan, cands []Candidate, days int) Plan {
	days = max(days, 1)
	byID := make(map[string]Candidate, len(cands))
	rank := make(map[string]int, len(cands))
	for i, c := range cands {
		byID[c.ID] = c
		rank[c.ID] = i
	}
	t := TargetsFor(cands, days)
	used := map[string]bool{}
	out := Plan{Days: make([]Day, days)}

	type bucket struct{ sights, meals []Item }
	buckets := make([]bucket, days)
	for i := 0; i < days; i++ {
		out.Days[i] = Day{Index: i, Title: fmt.Sprintf("Day %d", i+1)}
		if i >= len(p.Days) {
			continue
		}
		if title := strings.TrimSpace(p.Days[i].Title); title != "" {
			out.Days[i].Title = title
		}
		for _, it := range p.Days[i].Items {
			c, ok := byID[it.PlaceID]
			if !ok || used[it.PlaceID] {
				continue
			}
			if c.Kind == KindMeal {
				if len(buckets[i].meals) >= t.Meals {
					continue
				}
				buckets[i].meals = append(buckets[i].meals, Item{PlaceID: it.PlaceID, Note: it.Note})
			} else {
				if len(buckets[i].sights) >= t.Sights+1 {
					continue
				}
				slot := it.Slot
				if isMealSlot(slot) {
					slot = ""
				}
				buckets[i].sights = append(buckets[i].sights, Item{PlaceID: it.PlaceID, Slot: slot, Note: it.Note})
			}
			used[it.PlaceID] = true
		}
	}

	centroid := func(items []Item) (float64, float64, bool) {
		if len(items) == 0 {
			return 0, 0, false
		}
		var la, lo float64
		for _, it := range items {
			c := byID[it.PlaceID]
			la += c.Lat
			lo += c.Lon
		}
		return la / float64(len(items)), lo / float64(len(items)), true
	}
	// nearest unused candidate of the kind to a point; ties go to the better rank.
	nearest := func(kind string, lat, lon float64) (string, bool) {
		best, bestD := "", math.MaxFloat64
		for _, c := range cands {
			if used[c.ID] || (c.Kind == KindMeal) != (kind == KindMeal) {
				continue
			}
			d := metres(lat, lon, c.Lat, c.Lon)
			if d < bestD-1e-9 || (math.Abs(d-bestD) <= 1e-9 && rank[c.ID] < rank[best]) {
				best, bestD = c.ID, d
			}
		}
		return best, best != ""
	}
	best := func(kind string) (string, bool) {
		for _, c := range cands {
			if !used[c.ID] && (c.Kind == KindMeal) == (kind == KindMeal) {
				return c.ID, true
			}
		}
		return "", false
	}

	for i := range buckets {
		for len(buckets[i].sights) < t.Sights {
			var id string
			var ok bool
			if la, lo, has := centroid(buckets[i].sights); has {
				id, ok = nearest(KindSight, la, lo)
			} else {
				id, ok = best(KindSight)
			}
			if !ok {
				break
			}
			used[id] = true
			buckets[i].sights = append(buckets[i].sights, Item{PlaceID: id})
		}
	}
	for i := range buckets {
		for len(buckets[i].meals) < t.Meals {
			la, lo, has := centroid(buckets[i].sights)
			var id string
			var ok bool
			if has {
				id, ok = nearest(KindMeal, la, lo)
			} else {
				id, ok = best(KindMeal)
			}
			if !ok {
				break
			}
			used[id] = true
			buckets[i].meals = append(buckets[i].meals, Item{PlaceID: id})
		}
	}

	for i, b := range buckets {
		items := make([]Item, 0, len(b.sights)+len(b.meals))
		for j, it := range b.sights {
			if it.Slot == "" || isMealSlot(it.Slot) {
				it.Slot = sightSlot(j, len(b.sights))
			}
			items = append(items, it)
		}
		slots := []string{SlotLunch, SlotDinner}
		meals := append([]Item(nil), b.meals...)
		for j := range meals {
			if len(meals) == 1 && isMealSlot(meals[j].Slot) {
				continue // a lone meal keeps the slot the model chose
			}
			meals[j].Slot = slots[min(j, len(slots)-1)]
		}
		items = append(items, meals...)
		out.Days[i].Items = items
	}
	return out
}

func sightSlot(i, n int) string {
	switch {
	case n <= 1 || i < (n+1)/2:
		return SlotMorning
	case i == n-1 && n >= 4:
		return SlotEvening
	}
	return SlotAfternoon
}

// Finalise gives the plan's days and items their stable identities and dates. IDs are
// time-ordered UUIDs, so later edits and votes can refer to an item whatever moves.
func Finalise(p Plan, startDate string) Plan {
	start, _ := time.Parse("2006-01-02", startDate)
	out := Plan{Days: make([]Day, len(p.Days))}
	for i, d := range p.Days {
		d.Index = i
		d.DayID = newID()
		if !start.IsZero() {
			d.Date = start.AddDate(0, 0, i).Format("2006-01-02")
		}
		items := make([]Item, len(d.Items))
		for j, it := range d.Items {
			it.ItemID = newID()
			items[j] = it
		}
		d.Items = items
		out.Days[i] = d
	}
	return out
}

func newID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

func metres(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}
