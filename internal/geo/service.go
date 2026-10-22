package geo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sky-walkerx/trequila/internal/cache"
	"github.com/sky-walkerx/trequila/internal/metrics"
)

type Place struct {
	Name        string  `json:"name"`
	Category    string  `json:"category"`
	Address     string  `json:"address"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Description string  `json:"description,omitempty"`
}

// Coordinates holds lat/lon for caching
type Coordinates struct {
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	Found bool    `json:"found"`
}

// Overpass needs a generous server-side budget: dense cities (Tokyo, Paris) take
// well over 15s to gather, and when the [timeout:] budget is exceeded the API
// answers 504 with an HTML body instead of JSON.
const (
	overpassQueryTimeout  = 50 * time.Second
	overpassClientTimeout = 70 * time.Second
	overpassSearchRadius  = 10000
)

type Service struct {
	client         *http.Client
	overpassClient *http.Client
	redis          *redis.Client
	userAgent      string
	nominatimURL   string
	overpassURL    string
}

func getEnvOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func NewService(redisURL string) *Service {
	userAgent := getEnvOrDefault("HTTP_USER_AGENT", "Trequila/1.0 (https://github.com/sky-walkerx/trequila)")
	nominatim := getEnvOrDefault("OSM_NOMINATIM_URL", "https://nominatim.openstreetmap.org")
	overpass := getEnvOrDefault("OSM_OVERPASS_URL", "https://overpass-api.de/api/interpreter")

	return &Service{
		client:         &http.Client{Timeout: 15 * time.Second},
		overpassClient: &http.Client{Timeout: overpassClientTimeout},
		redis:          cache.NewRedisClient(redisURL),
		userAgent:      userAgent,
		nominatimURL:   nominatim,
		overpassURL:    overpass,
	}
}

// 1. Geocode: Convert "Paris" to Lat/Lon (with Redis caching)
func (s *Service) GetCoordinates(city string) (float64, float64, error) {
	ctx := context.Background()
	cacheKey := fmt.Sprintf("geo:coords:%s", city)

	if s.redis != nil {
		val, err := s.redis.Get(ctx, cacheKey).Result()
		if err == nil {
			var coords Coordinates
			if json.Unmarshal([]byte(val), &coords) == nil {
				if !coords.Found {
					slog.Debug("geocode cache hit (negative)", "city", city)
					return 0, 0, fmt.Errorf("location not found (cached)")
				}
				slog.Debug("geocode cache hit", "city", city)
				metrics.CacheHitsTotal.WithLabelValues("geo_coords").Inc()
				return coords.Lat, coords.Lon, nil
			}
		}
	}

	slog.Debug("geocode cache miss; fetching", "city", city)
	metrics.CacheMissesTotal.WithLabelValues("geo_coords").Inc()

	safeCity := url.QueryEscape(city)
	apiURL := fmt.Sprintf("%s/search?q=%s&format=json&limit=1", strings.TrimRight(s.nominatimURL, "/"), safeCity)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", s.userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	var result []struct {
		Lat string `json:"lat"`
		Lon string `json:"lon"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, err
	}

	if len(result) == 0 {
		if s.redis != nil {
			slog.Debug("caching geocode not-found", "city", city)
			data, _ := json.Marshal(Coordinates{Found: false})
			s.redis.Set(ctx, cacheKey, data, 1*time.Hour)
		}
		return 0, 0, fmt.Errorf("location not found")
	}

	lat, latErr := strconv.ParseFloat(result[0].Lat, 64)
	lon, lonErr := strconv.ParseFloat(result[0].Lon, 64)
	if latErr != nil || lonErr != nil {
		return 0, 0, fmt.Errorf("geocoder returned unparseable coordinates %q, %q", result[0].Lat, result[0].Lon)
	}

	if s.redis != nil {
		data, _ := json.Marshal(Coordinates{Lat: lat, Lon: lon, Found: true})
		s.redis.Set(ctx, cacheKey, data, 7*24*time.Hour)
	}

	// Sleep slightly to respect Nominatim 1 req/s
	time.Sleep(1 * time.Second)

	return lat, lon, nil
}

// 2. SearchPlaces: Find hotels/restaurants near coordinates (with Redis caching)
func (s *Service) SearchPlaces(lat, lon float64, categories string, limit int) ([]Place, error) {
	ctx := context.Background()
	cacheKey := fmt.Sprintf("geo:places:%.3f:%.3f:%s:%d", lat, lon, categories, limit)

	if s.redis != nil {
		val, err := s.redis.Get(ctx, cacheKey).Result()
		if err == nil {
			var places []Place
			if json.Unmarshal([]byte(val), &places) == nil {
				slog.Debug("places cache hit", "categories", categories, "lat", lat, "lon", lon)
				metrics.CacheHitsTotal.WithLabelValues("geo_places").Inc()
				return places, nil
			}
		}
	}

	slog.Debug("places cache miss; fetching from overpass", "categories", categories)
	metrics.CacheMissesTotal.WithLabelValues("geo_places").Inc()

	query := buildOverpassQuery(lat, lon, categories, limit)

	body, err := s.runOverpassQuery(ctx, query)
	if err != nil {
		return nil, err
	}

	var result struct {
		Elements []struct {
			Lat  float64           `json:"lat"`
			Lon  float64           `json:"lon"`
			Tags map[string]string `json:"tags"`
			Type string            `json:"type"`
			// For ways, center coordinates are returned in Center object
			Center *struct {
				Lat float64 `json:"lat"`
				Lon float64 `json:"lon"`
			} `json:"center"`
		} `json:"elements"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("overpass returned unparseable body: %w", err)
	}

	places := make([]Place, 0, len(result.Elements))
	for _, el := range result.Elements {
		name := el.Tags["name"]
		if name == "" {
			name = el.Tags["name:en"]
		}
		if name == "" {
			continue
		}

		pLat, pLon := el.Lat, el.Lon
		if el.Type == "way" && el.Center != nil {
			pLat, pLon = el.Center.Lat, el.Center.Lon
		}

		places = append(places, Place{
			Name:     name,
			Category: elementCategory(el.Tags, categories),
			Address:  strings.TrimSpace(el.Tags["addr:street"] + " " + el.Tags["addr:housenumber"]),
			Lat:      pLat,
			Lon:      pLon,
		})
	}

	if s.redis != nil && len(places) > 0 {
		data, _ := json.Marshal(places)
		s.redis.Set(ctx, cacheKey, data, 7*24*time.Hour)
	}

	return places, nil
}

// buildOverpassQuery renders the Overpass QL for a category group. Only named
// elements are gathered — unnamed ones are discarded during parsing anyway, and
// excluding them server-side keeps the `out` budget useful and the query fast.
func buildOverpassQuery(lat, lon float64, categories string, limit int) string {
	var tags []string
	for _, c := range strings.Split(categories, ",") {
		c = strings.TrimSpace(c)
		switch {
		case strings.Contains(c, "accommodation"):
			tags = append(tags, `["tourism"~"^(hotel|hostel|motel|guest_house)$"]`)
		case strings.Contains(c, "catering"):
			tags = append(tags, `["amenity"~"^(restaurant|cafe|bar|fast_food)$"]`)
		case strings.Contains(c, "tourism"), strings.Contains(c, "entertainment"):
			tags = append(tags, `["tourism"~"^(museum|gallery|attraction|viewpoint)$"]`, `["historic"]`)
		}
	}
	if len(tags) == 0 {
		tags = append(tags, `["tourism"]`, `["amenity"~"^(restaurant|cafe)$"]`)
	}

	var q strings.Builder
	fmt.Fprintf(&q, "[out:json][timeout:%d];\n(\n", int(overpassQueryTimeout.Seconds()))
	for _, tag := range tags {
		fmt.Fprintf(&q, "  nwr%s[\"name\"](around:%d,%f,%f);\n", tag, overpassSearchRadius, lat, lon)
	}
	fmt.Fprintf(&q, ");\nout center %d;\n", limit)
	return q.String()
}

// elementCategory derives a "group.kind" label (e.g. "accommodation.hotel") so the
// UI can badge each place. Falls back to the requested category group.
func elementCategory(tags map[string]string, requested string) string {
	group := strings.TrimSpace(strings.Split(requested, ",")[0])
	if i := strings.Index(group, "."); i >= 0 {
		group = group[:i]
	}
	for _, key := range []string{"tourism", "amenity", "historic"} {
		if v := tags[key]; v != "" {
			return group + "." + v
		}
	}
	return group
}

// runOverpassQuery POSTs the query and returns the raw body, retrying once on the
// transient statuses Overpass uses when it is overloaded (429 slot exhaustion,
// 504 gateway timeout). Non-2xx responses carry HTML, so the status must be
// checked before attempting to decode JSON.
func (s *Service) runOverpassQuery(ctx context.Context, query string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		reqCtx, cancel := context.WithTimeout(ctx, overpassClientTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, s.overpassURL, bytes.NewBufferString(query))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", s.userAgent)

		resp, err := s.overpassClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		cancel()

		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return body, nil
		}

		lastErr = fmt.Errorf("overpass returned HTTP %d: %s", resp.StatusCode, snippet(body))
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
