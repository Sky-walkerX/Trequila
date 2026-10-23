package scoring

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sky-walkerx/trequila/internal/osm"
	"github.com/sky-walkerx/trequila/internal/popularity"
	"github.com/sky-walkerx/trequila/internal/weather"
)

var center = Point{Lat: 38.7139, Lon: -9.1334}

// Mon 2026-12-07 .. Wed 2026-12-09
var trip = []time.Time{
	time.Date(2026, 12, 7, 0, 0, 0, 0, time.UTC),
	time.Date(2026, 12, 8, 0, 0, 0, 0, time.UTC),
	time.Date(2026, 12, 9, 0, 0, 0, 0, time.UTC),
}

func sight(ref, name, category string) Candidate {
	return Candidate{Ref: ref, Name: name, Kind: osm.KindSight, Category: category, Lat: center.Lat, Lon: center.Lon}
}

func meal(ref, name, category string) Candidate {
	return Candidate{Ref: ref, Name: name, Kind: osm.KindMeal, Category: category, Lat: center.Lat, Lon: center.Lon}
}

func refs(s []Scored) string {
	var r []string
	for _, x := range s {
		r = append(r, x.Ref)
	}
	return strings.Join(r, ",")
}

func find(s []Scored, ref string) *Scored {
	for i := range s {
		if s[i].Ref == ref {
			return &s[i]
		}
	}
	return nil
}

func hasReason(s *Scored, part string) bool {
	if s == nil {
		return false
	}
	for _, r := range s.Reasons {
		if strings.Contains(r, part) {
			return true
		}
	}
	return false
}

func TestMorePopularPlacesRankHigherAndSayWhy(t *testing.T) {
	famous := sight("node/1", "Famous Tower", "attraction")
	famous.Signals = popularity.Signals{Sitelinks: 80, Pageviews: 900000}
	obscure := sight("node/2", "Some Statue", "artwork")

	res := Rank(Input{Places: []Candidate{obscure, famous}, Dates: trip, Center: center})

	if refs(res.Sights) != "node/1,node/2" {
		t.Fatalf("order = %s", refs(res.Sights))
	}
	if !hasReason(&res.Sights[0], "80 Wikipedia languages") || !hasReason(&res.Sights[0], "900k") {
		t.Errorf("reasons = %v", res.Sights[0].Reasons)
	}
	if res.Sights[0].Score <= res.Sights[1].Score {
		t.Errorf("scores %v <= %v", res.Sights[0].Score, res.Sights[1].Score)
	}
}

func TestInterestsLiftMatchingCategories(t *testing.T) {
	museum := sight("node/1", "City Museum", "museum")
	park := sight("node/2", "Green Park", "park")

	res := Rank(Input{Places: []Candidate{museum, park}, Interests: []string{"Nature walks"}, Dates: trip, Center: center})

	if refs(res.Sights) != "node/2,node/1" || !hasReason(find(res.Sights, "node/2"), "nature") {
		t.Errorf("order = %s, reasons = %v", refs(res.Sights), find(res.Sights, "node/2").Reasons)
	}
}

func TestACloserPlaceBeatsAnOtherwiseIdenticalFarOne(t *testing.T) {
	near := sight("node/1", "Near Gardens", "garden")
	far := sight("node/2", "Far Gardens", "garden")
	far.Lat += 0.09 // ~10 km

	res := Rank(Input{Places: []Candidate{far, near}, Dates: trip, Center: center})
	if refs(res.Sights) != "node/1,node/2" {
		t.Errorf("order = %s", refs(res.Sights))
	}
}

func TestAPlaceClosedOnEveryTripDayIsExcludedWithAReason(t *testing.T) {
	closedMondays := sight("node/1", "Weekend Market", "marketplace")
	closedMondays.OpeningHours = "Sa,Su 09:00-14:00"
	open := sight("node/2", "Daily Museum", "museum")
	open.OpeningHours = "Mo-Su 10:00-18:00"

	res := Rank(Input{Places: []Candidate{closedMondays, open}, Dates: trip, Center: center})

	if find(res.Sights, "node/1") != nil {
		t.Error("a place closed on all three trip days must not be a candidate")
	}
	if len(res.Excluded) != 1 || res.Excluded[0].Ref != "node/1" || !strings.Contains(res.Excluded[0].Reason, "closed") {
		t.Errorf("excluded = %+v", res.Excluded)
	}
	if !hasReason(find(res.Sights, "node/2"), "open on all 3 trip days") {
		t.Errorf("reasons = %v", find(res.Sights, "node/2").Reasons)
	}
}

func TestPartlyOpenPlacesStayAndSayHowManyDays(t *testing.T) {
	c := sight("node/1", "Midweek Gallery", "gallery")
	c.OpeningHours = "Tu-Th 10:00-17:00"

	res := Rank(Input{Places: []Candidate{c}, Dates: trip, Center: center})
	if !hasReason(find(res.Sights, "node/1"), "open on 2 of 3 trip days") {
		t.Errorf("reasons = %v", res.Sights)
	}
}

func TestUnknownHoursAreKeptWithASmallPenaltyNeverTreatedAsClosed(t *testing.T) {
	unknown := sight("node/1", "Mystery Chapel", "attraction")
	verified := sight("node/2", "Known Chapel", "attraction")
	verified.OpeningHours = "Mo-Su 09:00-18:00"
	weird := sight("node/3", "Odd Chapel", "attraction")
	weird.OpeningHours = "sunrise-sunset"

	res := Rank(Input{Places: []Candidate{unknown, weird, verified}, Dates: trip, Center: center})

	if len(res.Sights) != 3 || len(res.Excluded) != 0 {
		t.Fatalf("sights=%d excluded=%v", len(res.Sights), res.Excluded)
	}
	if res.Sights[0].Ref != "node/2" {
		t.Errorf("verified hours should rank first, got %s", refs(res.Sights))
	}
	for _, ref := range []string{"node/1", "node/3"} {
		if !hasReason(find(res.Sights, ref), "opening hours unknown") {
			t.Errorf("%s reasons = %v", ref, find(res.Sights, ref).Reasons)
		}
	}
}

func TestRainyDaysFavourIndoorPlaces(t *testing.T) {
	museum := sight("node/1", "Rainy Museum", "museum")
	museum.Setting = osm.SettingIndoor
	hill := sight("node/2", "Windy Hill", "peak")
	hill.Setting = osm.SettingOutdoor
	wet := []weather.Day{
		{Date: "2026-12-07", Code: 63}, {Date: "2026-12-08", Code: 61}, {Date: "2026-12-09", Code: 0},
	}

	res := Rank(Input{Places: []Candidate{hill, museum}, Dates: trip, Days: wet, Center: center})

	if refs(res.Sights) != "node/1,node/2" || !hasReason(find(res.Sights, "node/1"), "indoor") {
		t.Errorf("order = %s, reasons = %v", refs(res.Sights), find(res.Sights, "node/1").Reasons)
	}
	dry := Rank(Input{Places: []Candidate{hill, museum}, Dates: trip, Center: center})
	if hasReason(find(dry.Sights, "node/1"), "indoor") {
		t.Error("without wet days there is no indoor bonus")
	}
}

func TestSelectionSizesAndMealsAreSeparate(t *testing.T) {
	var in []Candidate
	for i := 0; i < 40; i++ {
		c := sight(fmt.Sprintf("node/%d", i), fmt.Sprintf("Sight %d", i), []string{"museum", "park", "castle", "gallery"}[i%4])
		c.Lat += float64(i) * 0.001
		in = append(in, c)
	}
	for i := 0; i < 20; i++ {
		c := meal(fmt.Sprintf("node/m%d", i), fmt.Sprintf("Place %d", i), []string{"restaurant", "cafe", "bar"}[i%3])
		c.Lat += float64(i) * 0.001
		in = append(in, c)
	}

	res := Rank(Input{Places: in, Dates: trip, Center: center})

	if len(res.Sights) != 3*SightsPerDay || len(res.Meals) != 3*MealsPerDay {
		t.Errorf("sights=%d meals=%d, want %d and %d", len(res.Sights), len(res.Meals), 3*SightsPerDay, 3*MealsPerDay)
	}
}

func TestOneCategoryCannotTakeOverTheList(t *testing.T) {
	var in []Candidate
	for i := 0; i < 30; i++ {
		c := sight(fmt.Sprintf("node/c%d", i), fmt.Sprintf("Chapel %d", i), "chapel")
		c.Signals = popularity.Signals{Sitelinks: 50}
		c.Lat += float64(i) * 0.0005
		in = append(in, c)
	}
	for i := 0; i < 10; i++ {
		c := sight(fmt.Sprintf("node/o%d", i), fmt.Sprintf("Other %d", i), []string{"museum", "park"}[i%2])
		c.Lat += float64(i) * 0.0005
		in = append(in, c)
	}

	res := Rank(Input{Places: in, Dates: trip, Center: center})

	chapels := 0
	for _, s := range res.Sights {
		if s.Category == "chapel" {
			chapels++
		}
	}
	if max := (len(res.Sights)*2 + 4) / 5; chapels > max { // at most 40%, rounded up
		t.Errorf("%d of %d sights are chapels, cap is %d", chapels, len(res.Sights), max)
	}
	if len(res.Sights) != 3*SightsPerDay {
		t.Errorf("the cap must not shrink the list when other candidates exist: %d", len(res.Sights))
	}
}

func TestDuplicateNamesNearbyCollapseToTheBestOne(t *testing.T) {
	a := sight("node/1", "Café Central", "cafe")
	a.Signals = popularity.Signals{Sitelinks: 10}
	b := sight("node/2", "Cafe  central", "cafe") // same name once normalised, ~50 m away
	b.Lat += 0.0004
	far := sight("node/3", "Café Central", "cafe") // same name, a different branch across town
	far.Lat += 0.05

	res := Rank(Input{Places: []Candidate{b, far, a}, Dates: trip, Center: center})

	if find(res.Sights, "node/2") != nil || find(res.Sights, "node/1") == nil || find(res.Sights, "node/3") == nil {
		t.Errorf("selected = %s; want the better of the near pair plus the distant branch", refs(res.Sights))
	}
}

func TestRankingIsDeterministicWhateverTheInputOrder(t *testing.T) {
	var in []Candidate
	for i := 0; i < 60; i++ {
		kind := sight
		cat := []string{"museum", "park", "castle", "gallery", "viewpoint"}[i%5]
		if i%3 == 0 {
			kind, cat = meal, []string{"restaurant", "cafe", "bar"}[i%3]
		}
		c := kind(fmt.Sprintf("node/%d", i), fmt.Sprintf("Place %d", i), cat)
		c.Lat += float64(i%7) * 0.002 // many ties
		c.Signals = popularity.Signals{Sitelinks: i % 4}
		in = append(in, c)
	}
	want := Rank(Input{Places: in, Interests: []string{"history"}, Dates: trip, Center: center})

	for seed := int64(1); seed <= 5; seed++ {
		shuffled := append([]Candidate(nil), in...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := Rank(Input{Places: shuffled, Interests: []string{"history"}, Dates: trip, Center: center})
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: result differs\nwant sights %s\n got sights %s", seed, refs(want.Sights), refs(got.Sights))
		}
	}
}

func TestEveryCandidateCarriesAtLeastOneReason(t *testing.T) {
	res := Rank(Input{Places: []Candidate{sight("node/1", "Plain Place", "attraction"), meal("node/2", "Plain Diner", "restaurant")}, Dates: trip, Center: center})
	for _, s := range append(res.Sights, res.Meals...) {
		if len(s.Reasons) == 0 {
			t.Errorf("%s has no reason", s.Ref)
		}
	}
}

func TestFreeEntryIsNoted(t *testing.T) {
	c := sight("node/1", "Public Garden", "garden")
	c.Free = true
	res := Rank(Input{Places: []Candidate{c}, Dates: trip, Center: center})
	if !hasReason(find(res.Sights, "node/1"), "free") {
		t.Errorf("reasons = %v", res.Sights[0].Reasons)
	}
}
