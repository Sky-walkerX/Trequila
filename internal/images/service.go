// internal/images/service.go
package images

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sky-walkerx/trequila/internal/metrics"
)

type Image struct {
	URL          string `json:"url"`
	Photographer string `json:"photographer"`
	Alt          string `json:"alt"`
}

type Service struct {
	redis     *redis.Client
	apiKey    string
	rateLimit int           // Max requests
	window    time.Duration // Time window
}

func NewService(redisAddr, apiKey string) *Service {
	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	return &Service{
		redis:     rdb,
		apiKey:    apiKey,
		rateLimit: 190,           // Pexels limit is ~200/hr, stay safe
		window:    1 * time.Hour, // Reset every hour
	}
}

func (s *Service) GetImages(ctx context.Context, query string) ([]Image, error) {
	cacheKey := fmt.Sprintf("images:%s", query)

	// 1. Check Cache (Redis)
	val, err := s.redis.Get(ctx, cacheKey).Result()
	if err == nil {
		var images []Image
		if json.Unmarshal([]byte(val), &images) == nil {
			fmt.Println("Cache Hit for", query)
			metrics.CacheHitsTotal.WithLabelValues("images").Inc()
			return images, nil
		}
	}

	// 2. Check Rate Limit (Redis)
	if err := s.checkRateLimit(ctx); err != nil {
		return nil, err
	}

	// 3. Call External API (Pexels)
	fmt.Println("Fetching from Pexels for", query)
	metrics.CacheMissesTotal.WithLabelValues("images").Inc()
	images, err := s.fetchFromPexels(query)
	if err != nil {
		return nil, err
	}

	// 4. Save to Cache (7 Day TTL)
	data, _ := json.Marshal(images)
	s.redis.Set(ctx, cacheKey, data, 7*24*time.Hour)

	return images, nil
}

func (s *Service) checkRateLimit(ctx context.Context) error {
	// Simple sliding window counter
	key := "ratelimit:pexels"

	// Increment counter
	count, err := s.redis.Incr(ctx, key).Result()
	if err != nil {
		return err
	}

	// Set expiration on first request
	if count == 1 {
		s.redis.Expire(ctx, key, s.window)
	}

	if count > int64(s.rateLimit) {
		return fmt.Errorf("rate limit exceeded, try again later")
	}
	return nil
}

func (s *Service) fetchFromPexels(query string) ([]Image, error) {
	url := fmt.Sprintf("https://api.pexels.com/v1/search?query=%s&per_page=5&orientation=landscape", query)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", s.apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("pexels api error: %d", resp.StatusCode)
	}

	// Parse Pexels Response
	var result struct {
		Photos []struct {
			Src struct {
				Medium string `json:"medium"` // Use medium size
			} `json:"src"`
			Photographer string `json:"photographer"`
			Alt          string `json:"alt"`
		} `json:"photos"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var images []Image
	for _, p := range result.Photos {
		images = append(images, Image{
			URL:          p.Src.Medium,
			Photographer: p.Photographer,
			Alt:          p.Alt,
		})
	}
	return images, nil
}
