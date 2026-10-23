package grounding

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func cands(sights, meals int) []Candidate {
	var out []Candidate
	for i := 0; i < sights; i++ {
		out = append(out, Candidate{ID: fmt.Sprintf("node/s%d", i), Name: fmt.Sprintf("Sight %d", i), Kind: KindSight, Category: "museum",
			Lat: 38.7 + float64(i%4)*0.01, Lon: -9.1 + float64(i/4)*0.01, OpeningHours: "Mo-Su 10:00-18:00", Reasons: []string{"popular"}})
	}
	for i := 0; i < meals; i++ {
		out = append(out, Candidate{ID: fmt.Sprintf("node/m%d", i), Name: fmt.Sprintf("Meal %d", i), Kind: KindMeal, Category: "restaurant",
			Lat: 38.7 + float64(i%4)*0.01, Lon: -9.1 + float64(i/4)*0.01})
	}
	return out
}

func ids(p Plan) []string {
	var out []string
	for _, d := range p.Days {
		for _, it := range d.Items {
			out = append(out, it.PlaceID)
		}
	}
	return out
}

func TestPromptListsEveryCandidateAndTheRules(t *testing.T) {
	c := cands(4, 2)
	p := Prompt(PromptInput{Destination: "Lisbon", Days: 2, Candidates: c, Interests: []string{"food", "history"},
		WetDays: []int{1}})

	for _, want := range []string{"Lisbon", "2 days", "node/s0", "node/m1", "Sight 3", "Mo-Su 10:00-18:00", "food, history", "ONLY", "place_id", "day 2"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q:\n%s", want, p)
		}
	}
}

func TestParsePlanToleratesFencesAndRejectsGarbage(t *testing.T) {
	raw := "Here you go:\n```json\n{\"days\":[{\"title\":\"Old town\",\"items\":[{\"place_id\":\"node/s0\",\"slot\":\"morning\",\"note\":\"Go early\"}]}]}\n```"
	p, err := ParsePlan(raw)
	if err != nil || len(p.Days) != 1 || p.Days[0].Items[0].PlaceID != "node/s0" || p.Days[0].Title != "Old town" {
		t.Fatalf("plan = %+v, %v", p, err)
	}
	for _, bad := range []string{"", "I cannot help with that", `{"days": "nope"}`, `[]`, `{"days":[]}`} {
		if _, err := ParsePlan(bad); err == nil {
			t.Errorf("ParsePlan(%q) should fail", bad)
		}
	}
}

func goodPlan() Plan {
	return Plan{Days: []Day{
		{Title: "A", Items: []Item{{PlaceID: "node/s0"}, {PlaceID: "node/s1"}, {PlaceID: "node/s2"}, {PlaceID: "node/m0", Slot: SlotLunch}}},
		{Title: "B", Items: []Item{{PlaceID: "node/s3"}, {PlaceID: "node/s4"}, {PlaceID: "node/s5"}, {PlaceID: "node/m1", Slot: SlotLunch}}},
	}}
}

func TestValidateAcceptsAGroundedPlan(t *testing.T) {
	if probs := Validate(goodPlan(), cands(8, 4), 2); len(probs) != 0 {
		t.Errorf("problems = %v", probs)
	}
}

func TestValidateNamesEachKindOfProblem(t *testing.T) {
	c := cands(8, 4)
	cases := map[string]struct {
		mutate func(*Plan)
		want   string
	}{
		"unknown id":         {func(p *Plan) { p.Days[0].Items[0].PlaceID = "node/invented" }, "not one of the candidates"},
		"duplicate":          {func(p *Plan) { p.Days[1].Items[0].PlaceID = "node/s0" }, "already used"},
		"wrong day count":    {func(p *Plan) { p.Days = p.Days[:1] }, "2 days"},
		"sight in meal slot": {func(p *Plan) { p.Days[0].Items[0].Slot = SlotDinner }, "meal slot"},
		"meal as a sight":    {func(p *Plan) { p.Days[0].Items[3].Slot = SlotMorning }, "meal"},
		"too few":            {func(p *Plan) { p.Days[1].Items = p.Days[1].Items[:1] }, "at least"},
	}
	for name, tc := range cases {
		p := goodPlan()
		tc.mutate(&p)
		probs := Validate(p, c, 2)
		if len(probs) == 0 {
			t.Errorf("%s: no problem reported", name)
			continue
		}
		found := false
		for _, pr := range probs {
			if strings.Contains(pr.Message, tc.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: problems %v do not mention %q", name, probs, tc.want)
		}
	}
}

func TestValidateRejectsADayWithTooManySights(t *testing.T) {
	var items []Item
	for i := 0; i < 7; i++ {
		items = append(items, Item{PlaceID: fmt.Sprintf("node/s%d", i)})
	}
	probs := Validate(Plan{Days: []Day{{Items: items}}}, cands(8, 4), 1)
	if len(probs) != 1 || !strings.Contains(probs[0].Message, "at most") {
		t.Errorf("problems = %v", probs)
	}
}

func TestRepairPromptQuotesTheProblemsAndThePlan(t *testing.T) {
	p := goodPlan()
	p.Days[0].Items[0].PlaceID = "node/invented"
	probs := Validate(p, cands(8, 4), 2)
	rp := RepairPrompt(p, probs)
	if !strings.Contains(rp, "node/invented") || !strings.Contains(rp, "not one of the candidates") || !strings.Contains(rp, "place_id") {
		t.Errorf("repair prompt:\n%s", rp)
	}
}

func TestNormalizeAlwaysReturnsAValidPlanFromAnyInput(t *testing.T) {
	c := cands(14, 6)
	garbage := []Plan{
		{},
		{Days: []Day{{Items: []Item{{PlaceID: "node/invented"}, {PlaceID: "node/invented"}}}}},
		{Days: []Day{{Items: []Item{{PlaceID: "node/s0"}, {PlaceID: "node/s0"}}}, {Items: []Item{{PlaceID: "node/s0"}, {PlaceID: "node/m0", Slot: SlotMorning}}}, {}, {}, {}}},
		goodPlan(),
	}
	for i, in := range garbage {
		out := Normalize(in, c, 3)
		if probs := Validate(out, c, 3); len(probs) != 0 {
			t.Errorf("case %d: normalized plan still has problems: %v", i, probs)
		}
		if len(out.Days) != 3 {
			t.Errorf("case %d: %d days", i, len(out.Days))
		}
	}
}

func TestNormalizeKeepsWhatWasValidAndFillsTheRest(t *testing.T) {
	c := cands(14, 6)
	in := goodPlan()
	in.Days[0].Items[1].PlaceID = "node/invented"
	out := Normalize(in, c, 3)

	got := ids(out)
	for _, keep := range []string{"node/s0", "node/s2", "node/m0", "node/s3"} {
		if !contains(got, keep) {
			t.Errorf("valid item %s was dropped: %v", keep, got)
		}
	}
	if contains(got, "node/invented") {
		t.Error("an invented id survived")
	}
	if out.Days[0].Title != "A" {
		t.Errorf("a valid day's title should be kept, got %q", out.Days[0].Title)
	}
}

func TestNormalizeWithFewCandidatesUsesAllOfThemAndStaysValid(t *testing.T) {
	c := cands(3, 1)
	out := Normalize(Plan{}, c, 3)
	if probs := Validate(out, c, 3); len(probs) != 0 {
		t.Errorf("problems: %v", probs)
	}
	if len(ids(out)) != 4 {
		t.Errorf("used %v, want all 4 candidates", ids(out))
	}
}

func TestNormalizeIsDeterministicAndGroupsNearbyPlaces(t *testing.T) {
	c := cands(14, 6)
	a := Normalize(Plan{}, c, 3)
	shuffledIn := append([]Candidate(nil), c...)
	rand.New(rand.NewSource(7)).Shuffle(len(shuffledIn), func(i, j int) { shuffledIn[i], shuffledIn[j] = shuffledIn[j], shuffledIn[i] })
	b := Normalize(Plan{}, c, 3)
	if !reflect.DeepEqual(ids(a), ids(b)) {
		t.Error("two runs on the same input differ")
	}
	// Candidates are ranked best-first by the caller, so the first day is anchored on the best sight.
	if ids(a)[0] != "node/s0" {
		t.Errorf("first day should start from the top-ranked sight, got %v", ids(a)[:3])
	}
}

func TestFinaliseAssignsStableUniqueIDs(t *testing.T) {
	c := cands(14, 6)
	p := Finalise(Normalize(Plan{}, c, 3), "2026-12-07")
	seen := map[string]bool{}
	for i, d := range p.Days {
		if d.DayID == "" || seen[d.DayID] {
			t.Errorf("day %d id %q missing or duplicate", i, d.DayID)
		}
		seen[d.DayID] = true
		if d.Date != fmt.Sprintf("2026-12-%02d", 7+i) {
			t.Errorf("day %d date = %q", i, d.Date)
		}
		for _, it := range d.Items {
			if it.ItemID == "" || seen[it.ItemID] {
				t.Errorf("item id %q missing or duplicate", it.ItemID)
			}
			seen[it.ItemID] = true
		}
	}
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}
