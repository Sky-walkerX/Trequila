package images

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sky-walkerx/trequila/internal/cache"
	"github.com/sky-walkerx/trequila/internal/metrics"
)

type Image struct {
	URL          string `json:"url"`
	Photographer string `json:"photographer"`
	Alt          string `json:"alt"`
}

type Service struct {
	redis        *redis.Client
	client       *http.Client
	userAgent    string
	wikipediaURL string
	openverseURL string
}

func getEnvOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func NewService(redisURL string) *Service {
	rdb := cache.NewRedisClient(redisURL)

	userAgent := getEnvOrDefault("HTTP_USER_AGENT", "Trequila/1.0 (https://github.com/sky-walkerx/trequila)")
	wikiURL := getEnvOrDefault("WIKIPEDIA_API_URL", "https://en.wikipedia.org/api/rest_v1")
	openverseURL := getEnvOrDefault("OPENVERSE_API_URL", "https://api.openverse.engineering/v1")

	return &Service{
		redis:        rdb,
		client:       &http.Client{Timeout: 10 * time.Second},
		userAgent:    userAgent,
		wikipediaURL: wikiURL,
		openverseURL: openverseURL,
	}
}

func (s *Service) GetImages(ctx context.Context, query string) ([]Image, error) {
	cacheKey := fmt.Sprintf("images:%s", query)

	if s.redis != nil {
		val, err := s.redis.Get(ctx, cacheKey).Result()
		if err == nil {
			var images []Image
			if json.Unmarshal([]byte(val), &images) == nil {
				slog.Debug("images cache hit", "query", query)
				metrics.CacheHitsTotal.WithLabelValues("images").Inc()
				return images, nil
			}
		}
	}

	slog.Debug("fetching images", "query", query)
	metrics.CacheMissesTotal.WithLabelValues("images").Inc()

	var images []Image

	// 1. Wikipedia Hero Image
	wikiImg := s.fetchWikipediaImage(query)
	if wikiImg != nil {
		images = append(images, *wikiImg)
	}

	// 2. Openverse Gallery
	openverseImgs, _ := s.fetchOpenverseImages(query)
	images = append(images, openverseImgs...)

	if len(images) == 0 {
		return nil, fmt.Errorf("no images found")
	}

	if s.redis != nil {
		data, _ := json.Marshal(images)
		s.redis.Set(ctx, cacheKey, data, 7*24*time.Hour)
	}

	return images, nil
}

func (s *Service) fetchWikipediaImage(query string) *Image {
	safeQuery := url.PathEscape(query)
	apiURL := fmt.Sprintf("%s/page/summary/%s", strings.TrimRight(s.wikipediaURL, "/"), safeQuery)

	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("User-Agent", s.userAgent)

	resp, err := s.client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return nil
	}
	defer resp.Body.Close()

	var result struct {
		Title         string `json:"title"`
		OriginalImage struct {
			Source string `json:"source"`
		} `json:"originalimage"`
		Thumbnail struct {
			Source string `json:"source"`
		} `json:"thumbnail"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	src := result.OriginalImage.Source
	if src == "" {
		src = result.Thumbnail.Source
	}

	if src == "" {
		return nil
	}

	return &Image{
		URL:          src,
		Photographer: "Wikipedia/Wikimedia Commons",
		Alt:          result.Title,
	}
}

func (s *Service) fetchOpenverseImages(query string) ([]Image, error) {
	safeQuery := url.QueryEscape(query)
	apiURL := fmt.Sprintf("%s/images/?q=%s&page_size=5&license_type=all-cc", strings.TrimRight(s.openverseURL, "/"), safeQuery)

	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("User-Agent", s.userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openverse api error: %d", resp.StatusCode)
	}

	var result struct {
		Results []struct {
			URL     string `json:"url"`
			Creator string `json:"creator"`
			Title   string `json:"title"`
			License string `json:"license"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var images []Image
	for _, r := range result.Results {
		if r.URL == "" {
			continue
		}
		images = append(images, Image{
			URL:          r.URL,
			Photographer: fmt.Sprintf("%s (CC %s)", r.Creator, r.License),
			Alt:          r.Title,
		})
	}

	return images, nil
}
