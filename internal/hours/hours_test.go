package hours

import (
	"errors"
	"testing"
	"time"
)

// A fixed week: Monday 2 November 2026.
var week = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

func at(day, clock string) time.Time {
	offsets := map[string]int{"Mo": 0, "Tu": 1, "We": 2, "Th": 3, "Fr": 4, "Sa": 5, "Su": 6, "Mo2": 7}
	t, err := time.Parse("15:04", clock)
	if err != nil {
		panic(err)
	}
	d := week.AddDate(0, 0, offsets[day])
	return time.Date(d.Year(), d.Month(), d.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

func TestTheFixedWeekStartsOnAMonday(t *testing.T) {
	if week.Weekday() != time.Monday {
		t.Fatalf("test calendar is wrong: %v", week.Weekday())
	}
}

type probe struct {
	day, clock string
	want       State
}

func TestSchedules(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		probes []probe
	}{
		{"always open", "24/7", []probe{{"Mo", "00:00", Open}, {"Su", "23:59", Open}, {"We", "03:30", Open}}},
		{"weekdays only", "Mo-Fr 09:00-17:00", []probe{
			{"Mo", "08:59", Closed}, {"Mo", "09:00", Open}, {"Fr", "16:59", Open}, {"Fr", "17:00", Closed},
			{"Sa", "12:00", Closed}, {"Su", "12:00", Closed}}},
		{"two rules", "Mo-Fr 09:00-17:00; Sa 10:00-14:00", []probe{
			{"Sa", "09:59", Closed}, {"Sa", "10:00", Open}, {"Sa", "14:00", Closed}, {"Su", "11:00", Closed}}},
		{"split day", "Mo-Fr 09:00-12:00,14:00-18:00", []probe{
			{"Tu", "11:59", Open}, {"Tu", "12:00", Closed}, {"Tu", "13:59", Closed}, {"Tu", "14:00", Open}, {"Tu", "18:00", Closed}}},
		{"comma and space between ranges", "Mo-Fr 09:00-12:00, 14:00-18:00", []probe{{"We", "10:00", Open}, {"We", "13:00", Closed}, {"We", "15:00", Open}}},
		{"day list", "Mo,We,Fr 10:00-16:00", []probe{{"Mo", "12:00", Open}, {"Tu", "12:00", Closed}, {"We", "12:00", Open}}},
		{"mixed list and range", "Mo,We-Fr 10:00-16:00", []probe{{"Tu", "12:00", Closed}, {"Th", "12:00", Open}}},
		{"wrap-around range", "Fr-Mo 10:00-22:00", []probe{{"Fr", "12:00", Open}, {"Su", "12:00", Open}, {"Mo", "12:00", Open}, {"Tu", "12:00", Closed}, {"Th", "12:00", Closed}}},
		{"later rule overrides", "Mo-Su 09:00-18:00; We off", []probe{{"Tu", "10:00", Open}, {"We", "10:00", Closed}, {"Th", "10:00", Open}}},
		{"override with other hours", "Mo-Fr 09:00-17:00; Fr 09:00-13:00", []probe{{"Th", "16:00", Open}, {"Fr", "12:00", Open}, {"Fr", "14:00", Closed}}},
		{"closed keyword", "Mo-Sa 10:00-18:00; Su closed", []probe{{"Su", "12:00", Closed}, {"Sa", "12:00", Open}}},
		{"no day selector means every day", "09:00-17:00", []probe{{"Mo", "10:00", Open}, {"Su", "10:00", Open}, {"Su", "18:00", Closed}}},
		{"overnight stays open past midnight", "Fr,Sa 18:00-02:00", []probe{
			{"Fr", "17:59", Closed}, {"Fr", "18:00", Open}, {"Fr", "23:59", Open}, {"Sa", "01:59", Open}, {"Sa", "02:00", Closed},
			{"Sa", "23:00", Open}, {"Su", "01:00", Open}, {"Su", "02:30", Closed}, {"Mo", "01:00", Closed}}},
		{"full day with 24:00", "Mo-Su 00:00-24:00", []probe{{"We", "00:00", Open}, {"We", "23:59", Open}}},
		{"public holiday clause is ignored", "Mo-Fr 09:00-17:00; PH off", []probe{{"Mo", "10:00", Open}, {"Sa", "10:00", Closed}}},
		{"public holiday in the day list", "Mo-Fr,PH 09:00-17:00", []probe{{"Mo", "10:00", Open}, {"Sa", "10:00", Closed}}},
		{"school holiday clause is ignored", "Mo-Fr 08:00-16:00; SH off", []probe{{"Tu", "09:00", Open}}},
		{"single digit hour", "Mo-Fr 9:00-17:30", []probe{{"Mo", "09:00", Open}, {"Mo", "17:29", Open}, {"Mo", "17:30", Closed}}},
		{"comma separated rules", "Mo-Fr 09:00-17:00, Sa 10:00-14:00", []probe{{"Sa", "11:00", Open}, {"Su", "11:00", Closed}}},
		{"extra whitespace and trailing semicolon", "  Mo-Fr   09:00-17:00 ;  ", []probe{{"Mo", "10:00", Open}}},
		{"lowercase day names are not valid OSM but common", "mo-fr 09:00-17:00", []probe{{"Mo", "10:00", Open}}},
		{"museum closed on monday", "Tu-Su 10:00-18:00", []probe{{"Mo", "12:00", Closed}, {"Tu", "12:00", Open}}},
		{"restaurant late night", "Mo-Th 12:00-23:00; Fr-Sa 12:00-01:00; Su 12:00-22:00", []probe{
			{"Fr", "00:30", Closed}, {"Sa", "00:30", Open}, {"Su", "00:30", Open}, {"Mo2", "00:30", Closed}, {"Su", "22:00", Closed}}},
		{"typical shop", "Mo-Fr 08:30-19:00; Sa 09:00-18:00", []probe{{"Sa", "17:59", Open}, {"Sa", "18:00", Closed}}},
		{"park", "24/7", []probe{{"Th", "02:00", Open}}},
		{"sunday only", "Su 11:00-15:00", []probe{{"Su", "12:00", Open}, {"Sa", "12:00", Closed}}},
		{"all days listed individually", "Mo 10:00-12:00; Tu 10:00-12:00; We 14:00-16:00", []probe{{"Mo", "11:00", Open}, {"We", "11:00", Closed}, {"We", "15:00", Open}, {"Th", "15:00", Closed}}},
	}
	for _, c := range cases {
		s, err := Parse(c.raw)
		if err != nil {
			t.Errorf("%s: Parse(%q): %v", c.name, c.raw, err)
			continue
		}
		for _, p := range c.probes {
			if got := s.StateAt(at(p.day, p.clock)); got != p.want {
				t.Errorf("%s: %q at %s %s = %v, want %v", c.name, c.raw, p.day, p.clock, got, p.want)
			}
		}
	}
}

func TestUnsupportedSyntaxIsReportedNotGuessed(t *testing.T) {
	for _, raw := range []string{
		"sunrise-sunset", "Mo-Fr sunrise-sunset", "Mo-Fr 09:00+", "Apr-Oct Mo-Fr 09:00-17:00", "week 1-53/2 Mo 09:00-12:00",
		"Mo-Fr 09:00-17:00 open", `"by appointment"`, "by appointment", "Mo-Fr 09:00-17:00 \"call first\"", "PH off", "Mo-Fr dawn-dusk",
		"easter off", "Mo-Fr 09:00-17:00 [1]", "", "   ", "gibberish", "Mo-Fr 25:00-26:00", "Mo-Fr 09:00-",
	} {
		_, err := Parse(raw)
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("Parse(%q) = %v, want ErrUnsupported", raw, err)
		}
	}
}

func TestEvaluateTurnsUnparseableHoursIntoUnknownNeverClosed(t *testing.T) {
	when := at("Mo", "12:00")
	if got := Evaluate("", when); got != Unknown {
		t.Errorf("empty hours = %v, want Unknown", got)
	}
	if got := Evaluate("sunrise-sunset", when); got != Unknown {
		t.Errorf("unsupported hours = %v, want Unknown", got)
	}
	if got := Evaluate("Mo-Fr 09:00-17:00", when); got != Open {
		t.Errorf("supported hours = %v, want Open", got)
	}
}

func TestWindowsListTheOpenIntervalsStartingOnADate(t *testing.T) {
	s, err := Parse("Mo-Fr 09:00-12:00,14:00-18:00; Sa 20:00-02:00")
	if err != nil {
		t.Fatal(err)
	}
	w := s.Windows(at("Tu", "00:00"))
	if len(w) != 2 || !w[0].Start.Equal(at("Tu", "09:00")) || !w[0].End.Equal(at("Tu", "12:00")) || !w[1].Start.Equal(at("Tu", "14:00")) {
		t.Fatalf("Tuesday windows = %+v", w)
	}
	sat := s.Windows(at("Sa", "00:00"))
	if len(sat) != 1 || !sat[0].End.Equal(at("Su", "02:00")) {
		t.Fatalf("Saturday's overnight window should end Sunday 02:00, got %+v", sat)
	}
	if got := s.Windows(at("Su", "00:00")); len(got) != 0 {
		t.Errorf("Sunday starts no window of its own, got %+v", got)
	}
}

func TestOpenOnCountsAnOvernightSpillIntoTheNextDay(t *testing.T) {
	s, _ := Parse("Fr 22:00-03:00")
	if !s.OpenOn(at("Fr", "00:00")) {
		t.Error("open on Friday")
	}
	if !s.OpenOn(at("Sa", "00:00")) {
		t.Error("Friday's hours run into Saturday, so it is open at some point on Saturday")
	}
	if s.OpenOn(at("Su", "00:00")) {
		t.Error("closed on Sunday")
	}
}

func TestEvaluationUsesTheWallClockOfTheTimeGiven(t *testing.T) {
	lisbon, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Skip("tz database not available")
	}
	s, _ := Parse("Mo-Su 10:00-18:00")
	// 17:30 in Lisbon is open even though the same instant is 16:30 UTC in winter or 16:30 UTC in summer.
	if s.StateAt(time.Date(2026, 7, 1, 17, 30, 0, 0, lisbon)) != Open {
		t.Error("17:30 local should be open")
	}
	if s.StateAt(time.Date(2026, 7, 1, 18, 30, 0, 0, lisbon)) != Closed {
		t.Error("18:30 local should be closed")
	}
}

func TestWindowsOnADaylightSavingChangeDayKeepTheirWallClockTimes(t *testing.T) {
	lisbon, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Skip("tz database not available")
	}
	s, _ := Parse("Mo-Su 09:00-17:00")
	// Clocks went forward on Sunday 29 March 2026.
	day := time.Date(2026, 3, 29, 0, 0, 0, 0, lisbon)
	w := s.Windows(day)
	if len(w) != 1 || w[0].Start.Hour() != 9 || w[0].End.Hour() != 17 {
		t.Fatalf("windows = %+v", w)
	}
	if got := w[0].End.Sub(w[0].Start); got != 8*time.Hour {
		t.Errorf("a 09:00-17:00 window on that day is %v long in real time, want 8h", got)
	}
}

// Strings as found in OSM data. Each must either parse or be reported unsupported; none may panic.
var realWorld = []string{
	"Mo-Fr 09:00-18:00; Sa 09:00-13:00", "Tu-Su 10:00-18:00", "Mo-Su 08:00-20:00", "Mo-Su,PH 09:00-17:00",
	"24/7", "Mo-Fr 07:00-20:00; Sa,Su 08:00-18:00", "Mo-Th 11:30-22:00; Fr-Sa 11:30-23:00; Su 12:00-21:00",
	"Tu-Su 09:30-17:00; PH off", "Mo-Fr 08:00-12:00,13:00-17:00", "Sa-Su 10:00-16:00", "Mo 14:00-18:00; Tu-Fr 10:00-18:00",
	"Apr-Oct: Mo-Su 09:00-19:00; Nov-Mar: Mo-Su 09:00-17:00", "sunrise-sunset", "Mo-Fr 09:00-17:00; Sa 09:00-12:00; Su,PH off",
	"Mo-Sa 10:00-20:00", "Th-Tu 12:00-23:00", "Mo-Su 00:00-24:00", "Fr 12:00-02:00", "Mo-Su 18:00-03:00",
	"Mo-Fr 10:00-19:00; Sa 10:00-18:00; Su off", "by appointment", "Mo-Fr 09:00-17:00 \"Mittagspause 12-13\"", "Mo,Tu,Th,Fr 09:00-18:00",
	"Su-Th 11:00-23:00; Fr,Sa 11:00-00:00", "Mo-Su 09:00-dusk", "Jun-Aug Mo-Su 10:00-22:00", "Mo-Fr 09:00-17:00; 2026 Dec 25 off",
	"Mo-Su 10:00-20:00; PH 10:00-14:00", "Mo-Fr 08:00-18:00 open", "Mo-Su 09:00-17:00 || \"on request\"", "09:00-17:00",
	"Mo-Fr 7:00-15:00", "Mo-Fr 09:00-12:30,13:30-17:00; Sa 09:00-12:00", "Mo off; Tu-Su 10:00-17:00",
}

func TestRealWorldStringsParseOrAreReportedUnsupported(t *testing.T) {
	supported := 0
	for _, raw := range realWorld {
		s, err := Parse(raw)
		if err != nil {
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("%q: unexpected error type %v", raw, err)
			}
			continue
		}
		supported++
		for _, d := range []string{"Mo", "Tu", "We", "Th", "Fr", "Sa", "Su"} {
			s.StateAt(at(d, "12:00"))
			s.Windows(at(d, "00:00"))
			s.OpenOn(at(d, "00:00"))
		}
	}
	if supported < len(realWorld)*2/3 {
		t.Errorf("only %d of %d real-world strings are supported; the subset should cover most", supported, len(realWorld))
	}
}

func FuzzParse(f *testing.F) {
	for _, raw := range realWorld {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		s, err := Parse(raw)
		if err != nil {
			return
		}
		s.StateAt(at("We", "12:00"))
		s.Windows(at("We", "00:00"))
		s.OpenOn(at("We", "00:00"))
	})
}
