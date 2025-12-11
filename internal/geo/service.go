package geo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type Place struct {
	Name        string  `json:"name"`
	Category    string  `json:"category"`
	Address     string  `json:"address"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Description string  `json:"description,omitempty"`
}

type Service struct {
	apiKey string
	client *http.Client
}

func NewService(apiKey string) *Service {
	return &Service{
		apiKey: apiKey,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// 1. Geocode: Convert "Paris" to Lat/Lon
func (s *Service) GetCoordinates(city string) (float64, float64, error) {
	safeCity := url.QueryEscape(city)
	url := fmt.Sprintf("https://api.geoapify.com/v1/geocode/search?text=%s&apiKey=%s&limit=1", safeCity, s.apiKey)

	resp, err := s.client.Get(url)
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

	return result.Features[0].Properties.Lat, result.Features[0].Properties.Lon, nil
}

// 2. SearchPlaces: Find hotels/restaurants near coordinates
func (s *Service) SearchPlaces(lat, lon float64, categories string, limit int) ([]Place, error) {
	// Radius: 10km (10000 meters)
	url := fmt.Sprintf(
		"https://api.geoapify.com/v2/places?categories=%s&filter=circle:%f,%f,10000&limit=%d&apiKey=%s",
		categories, lon, lat, limit, s.apiKey, // Note: Geoapify uses (lon, lat) order for circles!
	)

	resp, err := s.client.Get(url)
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
	return places, nil
}