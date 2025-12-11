package geo

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
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
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type Service struct {
	apiKey string
	client *http.Client
	redis  *redis.Client
}

func NewService(apiKey, redisURL string) *Service {
	var rdb *redis.Client

	if redisURL != "" {
		// Parse Redis URL (supports rediss:// for Heroku/TLS)
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			log.Printf("Failed to parse Redis URL, using as address: %v", err)
			// Fallback: treat as simple address (for local dev)
			rdb = redis.NewClient(&redis.Options{Addr: redisURL})
		} else {
			// Heroku Redis requires InsecureSkipVerify due to certificate mismatch
			if strings.HasPrefix(redisURL, "rediss://") {
				opt.TLSConfig = &tls.Config{InsecureSkipVerify: true}
			}
			rdb = redis.NewClient(opt)
		}
	}

	return &Service{
		apiKey: apiKey,
		client: &http.Client{Timeout: 10 * time.Second},
		redis:  rdb,
	}
}

// 1. Geocode: Convert "Paris" to Lat/Lon (with Redis caching)
func (s *Service) GetCoordinates(city string) (float64, float64, error) {
	ctx := context.Background()
	cacheKey := fmt.Sprintf("geo:coords:%s", city)

	// Check Redis cache first
	if s.redis != nil {
		val, err := s.redis.Get(ctx, cacheKey).Result()
		if err == nil {
			var coords Coordinates
			if json.Unmarshal([]byte(val), &coords) == nil {
				log.Printf("Cache HIT for coordinates: %s", city)
				metrics.CacheHitsTotal.WithLabelValues("geo_coords").Inc()
				return coords.Lat, coords.Lon, nil
			}
		}
	}

	log.Printf("Cache MISS for coordinates: %s - fetching from API", city)
	metrics.CacheMissesTotal.WithLabelValues("geo_coords").Inc()

	safeCity := url.QueryEscape(city)
	apiURL := fmt.Sprintf("https://api.geoapify.com/v1/geocode/search?text=%s&apiKey=%s&limit=1", safeCity, s.apiKey)

	resp, err := s.client.Get(apiURL)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Features []struct {
			Properties struct {
				Lat float64 `json:"lat"`
				Lon float64 `json:"lon"`
			} `json:"properties"`
		} `json:"features"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, err
	}

	if len(result.Features) == 0 {
		return 0, 0, fmt.Errorf("location not found")
	}

	lat := result.Features[0].Properties.Lat
	lon := result.Features[0].Properties.Lon

	// Cache for 7 days (coordinates rarely change)
	if s.redis != nil {
		data, _ := json.Marshal(Coordinates{Lat: lat, Lon: lon})
		s.redis.Set(ctx, cacheKey, data, 7*24*time.Hour)
	}

	return lat, lon, nil
}

// 2. SearchPlaces: Find hotels/restaurants near coordinates (with Redis caching)
func (s *Service) SearchPlaces(lat, lon float64, categories string, limit int) ([]Place, error) {
	ctx := context.Background()
	// Cache key includes coordinates (rounded to 3 decimals ~100m precision) and category
	cacheKey := fmt.Sprintf("geo:places:%.3f:%.3f:%s:%d", lat, lon, categories, limit)

	// Check Redis cache first
	if s.redis != nil {
		val, err := s.redis.Get(ctx, cacheKey).Result()
		if err == nil {
			var places []Place
			if json.Unmarshal([]byte(val), &places) == nil {
				log.Printf("Cache HIT for places: %s at (%.3f, %.3f)", categories, lat, lon)
				metrics.CacheHitsTotal.WithLabelValues("geo_places").Inc()
				return places, nil
			}
		}
	}

	log.Printf("Cache MISS for places: %s - fetching from API", categories)
	metrics.CacheMissesTotal.WithLabelValues("geo_places").Inc()

	// Radius: 10km (10000 meters)
	apiURL := fmt.Sprintf(
		"https://api.geoapify.com/v2/places?categories=%s&filter=circle:%f,%f,10000&limit=%d&apiKey=%s",
		categories, lon, lat, limit, s.apiKey, // Note: Geoapify uses (lon, lat) order for circles!
	)

	resp, err := s.client.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Features []struct {
			Properties struct {
				Name     string  `json:"name"`
				Address  string  `json:"formatted"` // Full address
				Lat      float64 `json:"lat"`
				Lon      float64 `json:"lon"`
				Category string  `json:"datasource.raw.category"` // Simplified access
			} `json:"properties"`
		} `json:"features"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var places []Place
	for _, f := range result.Features {
		// Only keep places that actually have names
		if f.Properties.Name != "" {
			places = append(places, Place{
				Name:    f.Properties.Name,
				Address: f.Properties.Address,
				Lat:     f.Properties.Lat,
				Lon:     f.Properties.Lon,
			})
		}
	}

	// Cache for 7 days (places don't change frequently)
	if s.redis != nil && len(places) > 0 {
		data, _ := json.Marshal(places)
		s.redis.Set(ctx, cacheKey, data, 7*24*time.Hour)
	}

	return places, nil
}
