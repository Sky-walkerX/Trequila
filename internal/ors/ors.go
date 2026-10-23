// Package ors gives travel times between places. With an OpenRouteService key it asks
// ORS for real walking and driving times; without one, over quota, or when ORS is down,
// it estimates from straight-line distance and says so (Matrix.Estimated), so a trip is
// never blocked on a routing provider and an estimate is never passed off as a real time.
package ors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"
)

// Defaults.
const (
	DefaultMatrixURL     = "https://api.openrouteservice.org/v2/matrix"
	DefaultDirectionsURL = "https://api.openrouteservice.org/v2/directions"
	CacheTTL             = 7 * 24 * time.Hour

	maxMatrixPoints = 50 // the free plan's per-request limit
	maxBody         = 4 << 20

	// Estimates: straight line x 1.25 for the road, at plausible city speeds.
	detour       = 1.25
	walkSpeedMS  = 1.35 // ~4.9 km/h
	driveSpeedMS = 8.3  // ~30 km/h, door to door in a city
)

// Profile is a mode of travel.
type Profile string

const (
	Walking Profile = "foot-walking"
	Driving Profile = "driving-car"
)

// Point is a coordinate.
type Point struct{ Lat, Lon float64 }

// Matrix holds travel time (seconds) and distance (metres) between every pair of points.
type Matrix struct {
	Durations [][]float64 `json:"durations"`
	Distances [][]float64 `json:"distances"`
	Estimated bool        `json:"estimated"` // computed from straight-line distance, not routed
}

// Cache remembers routed matrices; failures behave like misses.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
}

// Config configures a Client. The zero value is usable and keyless.
type Config struct {
	Key           string // OpenRouteService API key; empty = estimates only
	MatrixURL     string
	DirectionsURL string
	Cache         Cache
	HTTP          *http.Client
	Log           *slog.Logger
}

// Client computes travel times.
type Client struct {
	key, matrixURL, directionsURL string
	cache                         Cache
	http                          *http.Client
	log                           *slog.Logger
}

// New builds a Client.
func New(cfg Config) *Client {
	c := &Client{key: cfg.Key, matrixURL: cfg.MatrixURL, directionsURL: cfg.DirectionsURL, cache: cfg.Cache, http: cfg.HTTP, log: cfg.Log}
	if c.matrixURL == "" {
		c.matrixURL = DefaultMatrixURL
	}
	if c.directionsURL == "" {
		c.directionsURL = DefaultDirectionsURL
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 20 * time.Second}
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c
}

// Matrix returns travel times between all pairs of pts. It never fails because ORS does:
// any problem yields an estimate with Estimated set.
func (c *Client) Matrix(ctx context.Context, profile Profile, pts []Point) (Matrix, error) {
	if c.key == "" || len(pts) < 2 || len(pts) > maxMatrixPoints {
		return Estimate(profile, pts), nil
	}
	key := cacheKey(profile, pts)
	if c.cache != nil {
		if raw, ok := c.cache.Get(ctx, key); ok {
			var m Matrix
			if json.Unmarshal(raw, &m) == nil && valid(m, len(pts)) {
				return m, nil
			}
		}
	}
	m, err := c.fetchMatrix(ctx, profile, pts)
	if err != nil {
		c.log.Warn("routing provider unavailable; using estimates", "error", err)
		return Estimate(profile, pts), nil
	}
	if c.cache != nil {
		if raw, err := json.Marshal(m); err == nil {
			c.cache.Set(ctx, key, raw, CacheTTL)
		}
	}
	return m, nil
}

func (c *Client) fetchMatrix(ctx context.Context, profile Profile, pts []Point) (Matrix, error) {
	locs := make([][2]float64, len(pts))
	for i, p := range pts {
		locs[i] = [2]float64{p.Lon, p.Lat} // ORS wants longitude first
	}
	body, _ := json.Marshal(map[string]any{"locations": locs, "metrics": []string{"duration", "distance"}})
	raw, err := c.post(ctx, fmt.Sprintf("%s/%s", strings.TrimRight(c.matrixURL, "/"), profile), body)
	if err != nil {
		return Matrix{}, err
	}
	var m Matrix
	if err := json.Unmarshal(raw, &m); err != nil {
		return Matrix{}, fmt.Errorf("ors: decode: %w", err)
	}
	if !valid(m, len(pts)) {
		return Matrix{}, fmt.Errorf("ors: matrix has the wrong shape for %d points", len(pts))
	}
	return m, nil
}

func valid(m Matrix, n int) bool {
	if len(m.Durations) != n || len(m.Distances) != n {
		return false
	}
	for i := 0; i < n; i++ {
		if len(m.Durations[i]) != n || len(m.Distances[i]) != n {
			return false
		}
	}
	return true
}

// Geometry returns the road path through pts, for drawing on a map. Without a key, or if
// ORS cannot answer, it returns nothing and no error: the map falls back to straight lines.
func (c *Client) Geometry(ctx context.Context, profile Profile, pts []Point) ([]Point, error) {
	if c.key == "" || len(pts) < 2 {
		return nil, nil
	}
	coords := make([][2]float64, len(pts))
	for i, p := range pts {
		coords[i] = [2]float64{p.Lon, p.Lat}
	}
	body, _ := json.Marshal(map[string]any{"coordinates": coords})
	raw, err := c.post(ctx, fmt.Sprintf("%s/%s/geojson", strings.TrimRight(c.directionsURL, "/"), profile), body)
	if err != nil {
		c.log.Warn("routing provider could not draw the path", "error", err)
		return nil, nil
	}
	var doc struct {
		Features []struct {
			Geometry struct {
				Coordinates [][]float64 `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Features) == 0 {
		return nil, nil
	}
	var line []Point
	for _, p := range doc.Features[0].Geometry.Coordinates {
		if len(p) >= 2 {
			line = append(line, Point{Lat: p[1], Lon: p[0]})
		}
	}
	return line, nil
}

func (c *Client) post(ctx context.Context, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ors: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ors: HTTP %d", resp.StatusCode) // the body can echo the request; keep it out
	}
	return raw, nil
}

func cacheKey(profile Profile, pts []Point) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s", profile)
	for _, p := range pts {
		fmt.Fprintf(h, "|%.5f,%.5f", p.Lat, p.Lon)
	}
	return "ors:matrix:v1:" + hex.EncodeToString(h.Sum(nil))[:32]
}

// Estimate builds a matrix from straight-line distances. Used when ORS cannot be.
func Estimate(profile Profile, pts []Point) Matrix {
	speed := walkSpeedMS
	if profile == Driving {
		speed = driveSpeedMS
	}
	n := len(pts)
	m := Matrix{Durations: make([][]float64, n), Distances: make([][]float64, n), Estimated: true}
	for i := range pts {
		m.Durations[i] = make([]float64, n)
		m.Distances[i] = make([]float64, n)
		for j := range pts {
			if i == j {
				continue
			}
			d := Haversine(pts[i], pts[j]) * detour
			m.Distances[i][j] = math.Round(d)
			m.Durations[i][j] = math.Round(d / speed)
		}
	}
	return m
}

// Haversine is the great-circle distance in metres.
func Haversine(a, b Point) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat, dLon := (b.Lat-a.Lat)*rad, (b.Lon-a.Lon)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(a.Lat*rad)*math.Cos(b.Lat*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(h))
}
