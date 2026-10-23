// Package osm fetches nearby places from OpenStreetMap through the Overpass API.
//
// One combined query returns every sight and eating place around a point. The client
// is built to survive Overpass's habits: it answers an overloaded request with an HTML
// page (sometimes with a 200), so a body that is not Overpass JSON is an error and never
// an empty result; it rations concurrent queries; it retries transient statuses once and
// then fails over to the next configured mirror; and it caches successful areas.
package osm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Defaults.
const (
	DefaultURL           = "https://overpass-api.de/api/interpreter"
	DefaultUserAgent     = "Trequila/1.0 (https://github.com/sky-walkerx/trequila)"
	defaultMaxConcurrent = 2
	defaultRetryDelay    = 2 * time.Second
	// queryTimeout is Overpass's server-side budget. Dense cities need far more than the
	// 15s of its default; past the budget it answers 504 with an HTML body.
	queryTimeout  = 50 * time.Second
	clientTimeout = 70 * time.Second
	// CacheTTL is how long an area is remembered. OSM edits are slow-moving and the
	// alerts job re-checks individual places by ID.
	CacheTTL = 30 * 24 * time.Hour
	// Result limits per kind; the scorer only needs the best few hundred of each.
	maxSights = 600
	maxMeals  = 400
	// maxBody bounds what is read from a mirror.
	maxBody = 32 << 20
)

// DefaultMirrors are tried in order when no URLs are configured. The main instance is
// often at capacity, so failing over is part of normal operation.
var DefaultMirrors = []string{
	DefaultURL,
	"https://overpass.kumi.systems/api/interpreter",
	"https://overpass.private.coffee/api/interpreter",
}

// ErrBadResponse means a mirror answered with something other than Overpass JSON.
var ErrBadResponse = errors.New("osm: overpass returned an unusable response")

// Cache remembers raw results between calls. A miss and a failure look the same, and a
// failing cache must never fail a lookup.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
}

// Config configures a Client. The zero value is usable.
type Config struct {
	URLs          []string      // Overpass mirrors, tried in order; empty = DefaultMirrors
	UserAgent     string        // sent on every request; empty = DefaultUserAgent
	MaxConcurrent int           // simultaneous queries; 0 = 2
	RetryDelay    time.Duration // pause before the single retry; 0 = 2s
	Cache         Cache         // optional
	HTTP          *http.Client  // optional; tests and callers with their own transport
	Log           *slog.Logger
}

// Client queries Overpass.
type Client struct {
	urls       []string
	userAgent  string
	retryDelay time.Duration
	cache      Cache
	http       *http.Client
	log        *slog.Logger
	slots      chan struct{}
}

// New builds a Client.
func New(cfg Config) *Client {
	c := &Client{
		urls:       cfg.URLs,
		userAgent:  cfg.UserAgent,
		retryDelay: cfg.RetryDelay,
		cache:      cfg.Cache,
		http:       cfg.HTTP,
		log:        cfg.Log,
	}
	if len(c.urls) == 0 {
		c.urls = DefaultMirrors
	}
	if c.userAgent == "" {
		c.userAgent = DefaultUserAgent
	}
	if c.retryDelay <= 0 {
		c.retryDelay = defaultRetryDelay
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: clientTimeout}
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	c.slots = make(chan struct{}, n)
	return c
}

// Element is one named OSM node, way or relation. Ways and relations are positioned at
// their centre.
type Element struct {
	Type string            `json:"type"` // node | way | relation
	ID   int64             `json:"id"`
	Name string            `json:"name"`
	Lat  float64           `json:"lat"`
	Lon  float64           `json:"lon"`
	Tags map[string]string `json:"tags,omitempty"`
}

// Ref is the element's stable OSM reference, e.g. "node/123".
func (e Element) Ref() string { return fmt.Sprintf("%s/%d", e.Type, e.ID) }

// Around returns the sights and eating places within radiusM metres of the point.
func (c *Client) Around(ctx context.Context, lat, lon float64, radiusM int) ([]Element, error) {
	key := cacheKey(lat, lon, radiusM)
	if c.cache != nil {
		if raw, ok := c.cache.Get(ctx, key); ok {
			var els []Element
			if json.Unmarshal(raw, &els) == nil {
				return els, nil
			}
		}
	}

	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	body, err := c.run(ctx, buildQuery(lat, lon, radiusM))
	if err != nil {
		return nil, err
	}
	els, err := parse(body)
	if err != nil {
		return nil, err
	}
	if c.cache != nil {
		if raw, err := json.Marshal(els); err == nil {
			c.cache.Set(ctx, key, raw, CacheTTL)
		}
	}
	return els, nil
}

// cacheKey rounds to about a kilometre, so near-identical searches share an entry.
func cacheKey(lat, lon float64, radiusM int) string {
	return fmt.Sprintf("osm:around:v1:%.2f,%.2f:%d", lat, lon, radiusM)
}

// run tries each mirror in turn; a transient failure is retried once on the same mirror
// first. Only a usable Overpass response ends the loop.
func (c *Client) run(ctx context.Context, query string) ([]byte, error) {
	var last error
	for _, u := range c.urls {
		for attempt := 0; attempt < 2; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(c.retryDelay):
				}
			}
			body, retry, err := c.once(ctx, u, query)
			if err == nil {
				return body, nil
			}
			last = err
			c.log.Warn("overpass request failed", "mirror", host(u), "attempt", attempt+1, "error", err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !retry {
				break
			}
		}
	}
	return nil, last
}

// once makes one request. retry says whether trying again could help.
func (c *Client) once(ctx context.Context, url, query string) (body []byte, retry bool, err error) {
	rctx, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewBufferString("data="+formEscape(query)))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("osm: request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, true, fmt.Errorf("osm: read body: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		if !looksLikeJSON(raw) {
			return nil, true, fmt.Errorf("%w: HTTP 200 with %q", ErrBadResponse, snippet(raw))
		}
		return raw, false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("osm: HTTP %d: %s", resp.StatusCode, snippet(raw))
	default:
		return nil, false, fmt.Errorf("osm: HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
}

func looksLikeJSON(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && b[0] == '{'
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func host(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

func formEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func parse(body []byte) ([]Element, error) {
	var doc struct {
		Elements []struct {
			Type   string  `json:"type"`
			ID     int64   `json:"id"`
			Lat    float64 `json:"lat"`
			Lon    float64 `json:"lon"`
			Center *struct {
				Lat float64 `json:"lat"`
				Lon float64 `json:"lon"`
			} `json:"center"`
			Tags map[string]string `json:"tags"`
		} `json:"elements"`
		Remark string `json:"remark"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	if doc.Elements == nil {
		// A runtime error is reported in "remark" next to an empty list; a genuinely empty
		// area still has "elements": [].
		return nil, fmt.Errorf("%w: no elements field (%s)", ErrBadResponse, snippet([]byte(doc.Remark)))
	}
	out := make([]Element, 0, len(doc.Elements))
	for _, r := range doc.Elements {
		name := strings.TrimSpace(r.Tags["name"])
		if name == "" {
			continue
		}
		lat, lon := r.Lat, r.Lon
		if r.Center != nil {
			lat, lon = r.Center.Lat, r.Center.Lon
		}
		if lat == 0 && lon == 0 {
			continue
		}
		out = append(out, Element{Type: r.Type, ID: r.ID, Name: name, Lat: lat, Lon: lon, Tags: r.Tags})
	}
	return out, nil
}

// buildQuery renders one Overpass QL query for everything a visitor might plan around.
// Sights and meals are limited separately: a city has far more places to eat than to
// see, and one shared limit would let the cafés crowd the sights out.
func buildQuery(lat, lon float64, radiusM int) string {
	around := fmt.Sprintf("(around:%d,%f,%f)", radiusM, lat, lon)
	var q strings.Builder
	fmt.Fprintf(&q, "[out:json][timeout:%d];\n(\n", int(queryTimeout.Seconds()))
	for _, filter := range []string{
		`["tourism"~"^(attraction|museum|gallery|viewpoint|artwork|zoo|theme_park|aquarium)$"]`,
		`["historic"]["historic"!~"^(wayside_cross|wayside_shrine|boundary_stone|milestone|manhole|tomb|charcoal_pile|bomb_crater)$"]`,
		`["leisure"~"^(park|garden|nature_reserve)$"]`,
		`["natural"~"^(beach|peak)$"]`,
		`["amenity"~"^(theatre|arts_centre|marketplace)$"]`,
	} {
		fmt.Fprintf(&q, "  nwr%s[\"name\"]%s;\n", filter, around)
	}
	q.WriteString(")->.sights;\n(\n")
	// Only eating places that say when they open or what they serve: they are the ones
	// whose data is good enough to schedule around.
	for _, extra := range []string{`["opening_hours"]`, `["cuisine"]`} {
		fmt.Fprintf(&q, "  nwr[\"amenity\"~\"^(restaurant|cafe|bar|pub|fast_food)$\"][\"name\"]%s%s;\n", extra, around)
	}
	fmt.Fprintf(&q, ")->.meals;\n.sights out center tags %d;\n.meals out center tags %d;", maxSights, maxMeals)
	return q.String()
}

// --- Element helpers ---

// Kind separates what a visitor sees from where they eat.
type Kind int

const (
	KindNone  Kind = iota // not something the planner uses
	KindSight             // a place to visit
	KindMeal              // somewhere to eat or drink
)

// Setting says whether a visit happens under a roof.
type Setting int

const (
	SettingUnknown Setting = iota
	SettingIndoor
	SettingOutdoor
)

var mealAmenities = map[string]bool{"restaurant": true, "cafe": true, "bar": true, "pub": true, "fast_food": true}

// Kind classifies the element.
func (e Element) Kind() Kind {
	if mealAmenities[e.Tags["amenity"]] {
		return KindMeal
	}
	if e.Category() != "" {
		return KindSight
	}
	return KindNone
}

// Category is the element's primary type in plain words ("museum", "castle", "park").
func (e Element) Category() string {
	for _, k := range []string{"tourism", "leisure", "natural", "amenity"} {
		if v := e.Tags[k]; v != "" {
			return v
		}
	}
	if v := e.Tags["historic"]; v != "" {
		if v == "yes" {
			return "historic"
		}
		return v
	}
	return ""
}

var (
	indoorCategories  = map[string]bool{"museum": true, "gallery": true, "aquarium": true, "theatre": true, "arts_centre": true, "marketplace": true}
	outdoorCategories = map[string]bool{"park": true, "garden": true, "nature_reserve": true, "beach": true, "peak": true, "viewpoint": true, "zoo": true, "theme_park": true}
)

// Setting reports whether the place is indoors, from the explicit indoor tag when
// present and from its category otherwise. Unknown is common and honest.
func (e Element) Setting() Setting {
	switch e.Tags["indoor"] {
	case "yes":
		return SettingIndoor
	case "no":
		return SettingOutdoor
	}
	switch c := e.Category(); {
	case indoorCategories[c]:
		return SettingIndoor
	case outdoorCategories[c]:
		return SettingOutdoor
	}
	return SettingUnknown
}

// OpeningHours is the raw opening_hours tag, or "".
func (e Element) OpeningHours() string { return strings.TrimSpace(e.Tags["opening_hours"]) }

var wikidataID = regexp.MustCompile(`^Q[1-9][0-9]*$`)

// Wikidata is the element's Wikidata item ID, or "" when absent or malformed.
func (e Element) Wikidata() string {
	if id := strings.TrimSpace(e.Tags["wikidata"]); wikidataID.MatchString(id) {
		return id
	}
	return ""
}

// Wikipedia splits the "lang:Title" tag. A tag without a language prefix names no page
// that can be looked up reliably, so it yields nothing.
func (e Element) Wikipedia() (lang, title string) {
	lang, title, ok := strings.Cut(strings.TrimSpace(e.Tags["wikipedia"]), ":")
	if !ok || lang == "" || title == "" || len(lang) > 8 {
		return "", ""
	}
	return lang, title
}

// Website is the element's website, preferring the plain tag.
func (e Element) Website() string {
	for _, k := range []string{"website", "contact:website", "url"} {
		if v := strings.TrimSpace(e.Tags[k]); v != "" {
			return v
		}
	}
	return ""
}

// Free reports that the place states it charges no fee.
func (e Element) Free() bool { return e.Tags["fee"] == "no" }

// Distance is the straight-line distance in metres from the point.
func (e Element) Distance(lat, lon float64) float64 { return haversine(e.Lat, e.Lon, lat, lon) }

func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}
