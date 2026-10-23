package weather

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fake struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string // path?query of every call
	geoJSON  string
}

func (f *fake) start() *httptest.Server {
	mux := http.NewServeMux()
	rec := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
			f.mu.Unlock()
			h(w, r)
		}
	}
	mux.HandleFunc("/geo/v1/search", rec(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(f.geoJSON))
	}))
	mux.HandleFunc("/fc/v1/forecast", rec(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		start, _ := time.Parse("2006-01-02", q.Get("start_date"))
		end, _ := time.Parse("2006-01-02", q.Get("end_date"))
		var dates, codes, tmax, tmin, prob []string
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			dates = append(dates, `"`+d.Format("2006-01-02")+`"`)
			codes = append(codes, "61")
			tmax = append(tmax, "21.5")
			tmin = append(tmin, "14.0")
			prob = append(prob, "80")
		}
		w.Write([]byte(`{"timezone":"Europe/Lisbon","daily":{"time":[` + strings.Join(dates, ",") + `],"weather_code":[` + strings.Join(codes, ",") +
			`],"temperature_2m_max":[` + strings.Join(tmax, ",") + `],"temperature_2m_min":[` + strings.Join(tmin, ",") +
			`],"precipitation_probability_max":[` + strings.Join(prob, ",") + `]}}`))
	}))
	mux.HandleFunc("/ar/v1/archive", rec(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		start, _ := time.Parse("2006-01-02", q.Get("start_date"))
		end, _ := time.Parse("2006-01-02", q.Get("end_date"))
		var dates, codes, tmax, tmin []string
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			dates = append(dates, `"`+d.Format("2006-01-02")+`"`)
			codes = append(codes, "1")
			tmax = append(tmax, "25")
			tmin = append(tmin, "17")
		}
		w.Write([]byte(`{"daily":{"time":[` + strings.Join(dates, ",") + `],"weather_code":[` + strings.Join(codes, ",") +
			`],"temperature_2m_max":[` + strings.Join(tmax, ",") + `],"temperature_2m_min":[` + strings.Join(tmin, ",") + `]}}`))
	}))
	s := httptest.NewServer(mux)
	f.t.Cleanup(s.Close)
	return s
}

func (f *fake) calls(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

var now = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

func newClient(t *testing.T, f *fake, fb Fallback) *Client {
	s := f.start()
	return New(Config{
		GeocodingURL: s.URL + "/geo/v1/search", ForecastURL: s.URL + "/fc/v1/forecast", ArchiveURL: s.URL + "/ar/v1/archive",
		Fallback: fb, Now: func() time.Time { return now },
	})
}

func day(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }

func TestAnImminentTripUsesTheForecast(t *testing.T) {
	f := &fake{t: t}
	c := newClient(t, f, nil)

	days, err := c.Forecast(context.Background(), 38.7, -9.1, "Europe/Lisbon", day("2026-10-25"), day("2026-10-27"))
	if err != nil || len(days) != 3 {
		t.Fatalf("got %d days, %v", len(days), err)
	}
	d := days[0]
	if d.Date != "2026-10-25" || d.Code != 61 || d.TMax != 21.5 || d.TMin != 14 || d.RainProb != 80 || d.Source != SourceForecast {
		t.Errorf("day = %+v", d)
	}
	if f.calls("/ar/") != 0 {
		t.Error("the archive must not be used for dates inside the forecast window")
	}
}

func TestADistantTripUsesTheSameDatesLastYearAndSaysSo(t *testing.T) {
	f := &fake{t: t}
	c := newClient(t, f, nil)

	days, err := c.Forecast(context.Background(), 38.7, -9.1, "Europe/Lisbon", day("2027-03-10"), day("2027-03-11"))
	if err != nil || len(days) != 2 {
		t.Fatalf("got %d days, %v", len(days), err)
	}
	if days[0].Date != "2027-03-10" || days[0].Source != SourceTypical || days[0].Code != 1 {
		t.Errorf("day = %+v; dates must stay the trip's own while the data is last year's", days[0])
	}
	if f.calls("/fc/") != 0 {
		t.Error("the forecast must not be asked for dates beyond its horizon")
	}
	if !strings.Contains(f.requests[0], "start_date=2026-03-10") {
		t.Errorf("archive request = %q, want the dates a year earlier", f.requests[0])
	}
}

func TestATripStraddlingTheHorizonUsesBothSources(t *testing.T) {
	f := &fake{t: t}
	c := newClient(t, f, nil)

	// now + 15 days = 2026-11-07
	days, err := c.Forecast(context.Background(), 38.7, -9.1, "Europe/Lisbon", day("2026-11-06"), day("2026-11-09"))
	if err != nil || len(days) != 4 {
		t.Fatalf("got %d days, %v", len(days), err)
	}
	var sources []Source
	for _, d := range days {
		sources = append(sources, d.Source)
	}
	want := []Source{SourceForecast, SourceForecast, SourceTypical, SourceTypical}
	for i := range want {
		if sources[i] != want[i] {
			t.Fatalf("sources = %v, want %v", sources, want)
		}
	}
}

func TestWet(t *testing.T) {
	cases := []struct {
		d    Day
		wet  bool
		name string
	}{
		{Day{Code: 0}, false, "clear"},
		{Day{Code: 51}, true, "drizzle"},
		{Day{Code: 95}, true, "thunderstorm"},
		{Day{Code: 3, RainProb: 60}, true, "likely rain"},
		{Day{Code: 3, RainProb: 59}, false, "unlikely rain"},
	}
	for _, tc := range cases {
		if tc.d.Wet() != tc.wet {
			t.Errorf("%s: Wet() = %v", tc.name, tc.d.Wet())
		}
	}
}

func TestGeocodeUsesOpenMeteoAndKeepsItsTimezone(t *testing.T) {
	f := &fake{t: t, geoJSON: `{"results":[{"name":"Lisbon","country":"Portugal","latitude":38.71667,"longitude":-9.13333,"timezone":"Europe/Lisbon"}]}`}
	c := newClient(t, f, nil)

	d, err := c.Geocode(context.Background(), "Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "Lisbon" || d.Country != "Portugal" || d.Lat != 38.71667 || d.Timezone != "Europe/Lisbon" {
		t.Errorf("destination = %+v", d)
	}
}

type nominatim struct {
	lat, lon float64
	err      error
	asked    []string
}

func (n *nominatim) GetCoordinates(city string) (float64, float64, error) {
	n.asked = append(n.asked, city)
	return n.lat, n.lon, n.err
}

func TestGeocodeFallsBackToNominatimAndLooksUpTheTimezone(t *testing.T) {
	f := &fake{t: t, geoJSON: `{}`} // Open-Meteo has no "results" key when nothing matches
	nom := &nominatim{lat: 38.7, lon: -9.1}
	c := newClient(t, f, nom)

	d, err := c.Geocode(context.Background(), "Lisboa, PT")
	if err != nil {
		t.Fatal(err)
	}
	if len(nom.asked) != 1 || d.Lat != 38.7 || d.Timezone != "Europe/Lisbon" || d.Name != "Lisboa, PT" {
		t.Errorf("destination = %+v, asked %v", d, nom.asked)
	}
}

func TestGeocodeReportsNotFound(t *testing.T) {
	f := &fake{t: t, geoJSON: `{}`}
	c := newClient(t, f, &nominatim{err: errors.New("location not found")})

	if _, err := c.Geocode(context.Background(), "Nowhereville"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	c = newClient(t, f, nil)
	if _, err := c.Geocode(context.Background(), "Nowhereville"); !errors.Is(err, ErrNotFound) {
		t.Errorf("without a fallback: err = %v, want ErrNotFound", err)
	}
	if _, err := c.Geocode(context.Background(), "  "); !errors.Is(err, ErrNotFound) {
		t.Errorf("blank name: err = %v, want ErrNotFound", err)
	}
}
