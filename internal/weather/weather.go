// Package weather resolves a destination name to coordinates and a time zone, and
// provides the weather for the trip's dates: the real forecast when the trip is close
// enough for one to exist, and otherwise what the same dates looked like last year,
// labelled as "typical" so nothing pretends to be a forecast.
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	DefaultGeocodingURL = "https://geocoding-api.open-meteo.com/v1/search"
	DefaultForecastURL  = "https://api.open-meteo.com/v1/forecast"
	DefaultArchiveURL   = "https://archive-api.open-meteo.com/v1/archive"
	DefaultUserAgent    = "Trequila/1.0 (https://github.com/sky-walkerx/trequila)"

	// forecastHorizonDays is how far ahead Open-Meteo forecasts.
	forecastHorizonDays = 15
	maxBody             = 4 << 20
)

// ErrNotFound means no place matches the name.
var ErrNotFound = errors.New("weather: destination not found")

// Source says where a day's weather came from.
type Source string

const (
	SourceForecast Source = "forecast"
	SourceTypical  Source = "typical" // the same date last year
)

// Day is one day's weather.
type Day struct {
	Date     string  `json:"date"` // the trip's date, YYYY-MM-DD
	Code     int     `json:"code"` // WMO weather code
	TMax     float64 `json:"tmax"`
	TMin     float64 `json:"tmin"`
	RainProb int     `json:"rain_prob"` // percent; only forecasts carry it
	Source   Source  `json:"source"`
}

// Wet reports a day on which outdoor plans should give way to indoor ones: rain or
// worse by WMO code, or a forecast that makes rain likely.
func (d Day) Wet() bool { return d.Code >= 51 || d.RainProb >= 60 }

// Destination is a geocoded place.
type Destination struct {
	Name     string  `json:"name"`
	Country  string  `json:"country,omitempty"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Timezone string  `json:"timezone"`
}

// Fallback finds coordinates when Open-Meteo's geocoder does not know a name. It is
// satisfied by geo.Service, which also owns Nominatim's rate limit.
type Fallback interface {
	GetCoordinates(city string) (lat, lon float64, err error)
}

// Config configures a Client. The zero value is usable.
type Config struct {
	GeocodingURL string
	ForecastURL  string
	ArchiveURL   string
	UserAgent    string
	Fallback     Fallback
	HTTP         *http.Client
	Now          func() time.Time
}

// Client talks to Open-Meteo.
type Client struct {
	geocodingURL, forecastURL, archiveURL, userAgent string
	fallback                                         Fallback
	http                                             *http.Client
	now                                              func() time.Time
}

// New builds a Client.
func New(cfg Config) *Client {
	c := &Client{
		geocodingURL: cfg.GeocodingURL, forecastURL: cfg.ForecastURL, archiveURL: cfg.ArchiveURL,
		userAgent: cfg.UserAgent, fallback: cfg.Fallback, http: cfg.HTTP, now: cfg.Now,
	}
	if c.geocodingURL == "" {
		c.geocodingURL = DefaultGeocodingURL
	}
	if c.forecastURL == "" {
		c.forecastURL = DefaultForecastURL
	}
	if c.archiveURL == "" {
		c.archiveURL = DefaultArchiveURL
	}
	if c.userAgent == "" {
		c.userAgent = DefaultUserAgent
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 15 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// Geocode resolves a destination name. Open-Meteo's geocoder answers first; when it
// does not know the name, the fallback (Nominatim) is asked and the time zone is looked
// up from the coordinates.
func (c *Client) Geocode(ctx context.Context, name string) (Destination, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Destination{}, ErrNotFound
	}
	q := url.Values{"name": {name}, "count": {"1"}, "format": {"json"}}
	var doc struct {
		Results []struct {
			Name      string  `json:"name"`
			Country   string  `json:"country"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
			Timezone  string  `json:"timezone"`
		} `json:"results"`
	}
	if err := c.getJSON(ctx, c.geocodingURL+"?"+q.Encode(), &doc); err != nil {
		return Destination{}, fmt.Errorf("weather: geocode: %w", err)
	}
	if len(doc.Results) > 0 {
		r := doc.Results[0]
		return Destination{Name: r.Name, Country: r.Country, Lat: r.Latitude, Lon: r.Longitude, Timezone: r.Timezone}, nil
	}

	if c.fallback == nil {
		return Destination{}, ErrNotFound
	}
	lat, lon, err := c.fallback.GetCoordinates(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return Destination{}, ErrNotFound
		}
		return Destination{}, fmt.Errorf("weather: fallback geocoder: %w", err)
	}
	tz, err := c.timezone(ctx, lat, lon)
	if err != nil {
		return Destination{}, err
	}
	return Destination{Name: name, Lat: lat, Lon: lon, Timezone: tz}, nil
}

// timezone asks Open-Meteo for the zone at a point; its forecast response names it.
func (c *Client) timezone(ctx context.Context, lat, lon float64) (string, error) {
	q := url.Values{
		"latitude": {fmt.Sprintf("%.4f", lat)}, "longitude": {fmt.Sprintf("%.4f", lon)},
		"timezone": {"auto"}, "forecast_days": {"1"}, "daily": {"weather_code"},
	}
	var doc struct {
		Timezone string `json:"timezone"`
	}
	if err := c.getJSON(ctx, c.forecastURL+"?"+q.Encode(), &doc); err != nil {
		return "", fmt.Errorf("weather: timezone: %w", err)
	}
	if doc.Timezone == "" {
		return "", errors.New("weather: no timezone in response")
	}
	return doc.Timezone, nil
}

// Forecast returns one Day per date from..to inclusive. Dates within the forecast
// horizon come from the forecast; later dates come from the same date a year earlier.
func (c *Client) Forecast(ctx context.Context, lat, lon float64, tz string, from, to time.Time) ([]Day, error) {
	if to.Before(from) {
		return nil, errors.New("weather: end date is before start date")
	}
	if tz == "" {
		tz = "auto"
	}
	today := c.now().UTC().Truncate(24 * time.Hour)
	horizon := today.AddDate(0, 0, forecastHorizonDays)
	from, to = from.UTC().Truncate(24*time.Hour), to.UTC().Truncate(24*time.Hour)

	var days []Day
	// Forecast part: from the later of (from, today) up to the earlier of (to, horizon).
	fcFrom, fcTo := from, minTime(to, horizon)
	if fcFrom.Before(today) {
		fcFrom = today
	}
	if !fcFrom.After(fcTo) {
		got, err := c.fetch(ctx, c.forecastURL, lat, lon, tz, fcFrom, fcTo, SourceForecast, 0)
		if err != nil {
			return nil, err
		}
		days = append(days, got...)
	}
	// Everything not covered above is typical weather: dates before today (already
	// past) and dates beyond the horizon.
	covered := map[string]bool{}
	for _, d := range days {
		covered[d.Date] = true
	}
	var typFrom, typTo time.Time
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if covered[d.Format("2006-01-02")] {
			continue
		}
		if typFrom.IsZero() {
			typFrom = d
		}
		typTo = d
	}
	if !typFrom.IsZero() {
		got, err := c.fetch(ctx, c.archiveURL, lat, lon, tz, typFrom, typTo, SourceTypical, -1)
		if err != nil {
			return nil, err
		}
		days = append(days, got...)
	}
	sortDays(days)
	return days, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func sortDays(d []Day) {
	slices.SortFunc(d, func(a, b Day) int { return strings.Compare(a.Date, b.Date) })
}

// fetch reads daily values for from..to. yearShift moves the request's dates (the
// archive is asked for last year) while the returned days keep the trip's dates.
func (c *Client) fetch(ctx context.Context, base string, lat, lon float64, tz string, from, to time.Time, src Source, yearShift int) ([]Day, error) {
	rf, rt := from.AddDate(yearShift, 0, 0), to.AddDate(yearShift, 0, 0)
	daily := "weather_code,temperature_2m_max,temperature_2m_min"
	if src == SourceForecast {
		daily += ",precipitation_probability_max"
	}
	q := url.Values{
		"latitude": {fmt.Sprintf("%.4f", lat)}, "longitude": {fmt.Sprintf("%.4f", lon)},
		"timezone": {tz}, "daily": {daily},
		"start_date": {rf.Format("2006-01-02")}, "end_date": {rt.Format("2006-01-02")},
	}
	var doc struct {
		Daily struct {
			Time     []string   `json:"time"`
			Code     []*float64 `json:"weather_code"`
			TMax     []*float64 `json:"temperature_2m_max"`
			TMin     []*float64 `json:"temperature_2m_min"`
			RainProb []*float64 `json:"precipitation_probability_max"`
		} `json:"daily"`
	}
	if err := c.getJSON(ctx, base+"?"+q.Encode(), &doc); err != nil {
		return nil, fmt.Errorf("weather: %s: %w", src, err)
	}
	at := func(s []*float64, i int) float64 {
		if i < len(s) && s[i] != nil {
			return *s[i]
		}
		return 0
	}
	out := make([]Day, 0, len(doc.Daily.Time))
	for i, ds := range doc.Daily.Time {
		date, err := time.Parse("2006-01-02", ds)
		if err != nil {
			continue
		}
		out = append(out, Day{
			Date: date.AddDate(-yearShift, 0, 0).Format("2006-01-02"),
			Code: int(at(doc.Daily.Code, i)), TMax: at(doc.Daily.TMax, i), TMin: at(doc.Daily.TMin, i),
			RainProb: int(at(doc.Daily.RainProb, i)), Source: src,
		})
	}
	return out, nil
}

func (c *Client) getJSON(ctx context.Context, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
