package ors

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var pts = []Point{{Lat: 38.7139, Lon: -9.1334}, {Lat: 38.7223, Lon: -9.1393}, {Lat: 38.6916, Lon: -9.2160}}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func TestMatrixPostsLonLatAndReadsDurationsAndDistances(t *testing.T) {
	var gotPath, gotAuth string
	var body struct {
		Locations [][]float64 `json:"locations"`
		Metrics   []string    `json:"metrics"`
	}
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		io.WriteString(w, `{"durations":[[0,100,900],[110,0,800],[950,820,0]],"distances":[[0,150,1300],[160,0,1100],[1350,1150,0]]}`)
	})
	c := New(Config{Key: "k", MatrixURL: s.URL + "/v2/matrix"})

	m, err := c.Matrix(context.Background(), Walking, pts)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v2/matrix/foot-walking" || gotAuth != "k" {
		t.Errorf("path %q auth %q", gotPath, gotAuth)
	}
	if len(body.Locations) != 3 || body.Locations[0][0] != -9.1334 || body.Locations[0][1] != 38.7139 {
		t.Errorf("locations must be [lon, lat]: %v", body.Locations)
	}
	if m.Estimated || m.Durations[0][2] != 900 || m.Distances[2][0] != 1350 {
		t.Errorf("matrix = %+v", m)
	}
}

func TestWithoutAKeyTheMatrixIsAnEstimateAndNoRequestIsMade(t *testing.T) {
	var calls atomic.Int32
	s := serve(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	c := New(Config{MatrixURL: s.URL})

	m, err := c.Matrix(context.Background(), Walking, pts)
	if err != nil || !m.Estimated || calls.Load() != 0 {
		t.Fatalf("estimated=%v err=%v calls=%d", m.Estimated, err, calls.Load())
	}
	// Straight line x 1.25, at walking pace: Praça do Comércio to Rossio is ~1 km.
	if d := m.Distances[0][1]; d < 900 || d > 1700 {
		t.Errorf("distance = %.0f m", d)
	}
	if m.Durations[0][1] <= 0 || m.Durations[0][0] != 0 {
		t.Errorf("durations = %v", m.Durations)
	}
	walk := m.Durations[0][2]
	drive, _ := c.Matrix(context.Background(), Driving, pts)
	if drive.Durations[0][2] >= walk {
		t.Errorf("driving (%.0fs) should be faster than walking (%.0fs)", drive.Durations[0][2], walk)
	}
}

func TestAnUpstreamFailureFallsBackToAnEstimate(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"quota":   func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "quota", http.StatusTooManyRequests) },
		"down":    func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusBadGateway) },
		"garbage": func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "<html>") },
		"shape":   func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"durations":[[0]]}`) },
	} {
		s := serve(t, h)
		c := New(Config{Key: "k", MatrixURL: s.URL})
		m, err := c.Matrix(context.Background(), Walking, pts)
		if err != nil || !m.Estimated || len(m.Durations) != 3 {
			t.Errorf("%s: estimated=%v err=%v size=%d", name, m.Estimated, err, len(m.Durations))
		}
	}
}

type memCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (c *memCache) Get(_ context.Context, k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}
func (c *memCache) Set(_ context.Context, k string, v []byte, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string][]byte{}
	}
	c.m[k] = v
}

func TestRealMatricesAreCachedButEstimatesAreNot(t *testing.T) {
	var calls atomic.Int32
	fail := atomic.Bool{}
	s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, "down", 500)
			return
		}
		io.WriteString(w, `{"durations":[[0,1,2],[1,0,3],[2,3,0]],"distances":[[0,1,2],[1,0,3],[2,3,0]]}`)
	})
	c := New(Config{Key: "k", MatrixURL: s.URL, Cache: &memCache{}})
	ctx := context.Background()

	fail.Store(true)
	if m, _ := c.Matrix(ctx, Walking, pts); !m.Estimated {
		t.Fatal("setup: want an estimate")
	}
	fail.Store(false)
	m, _ := c.Matrix(ctx, Walking, pts)
	if m.Estimated {
		t.Error("an earlier failure must not have been cached")
	}
	before := calls.Load()
	if again, _ := c.Matrix(ctx, Walking, pts); again.Estimated || calls.Load() != before {
		t.Errorf("second call hit the upstream (calls %d -> %d) or lost the real data", before, calls.Load())
	}
	if other, _ := c.Matrix(ctx, Driving, pts); calls.Load() != before+1 || other.Estimated {
		t.Errorf("a different profile is a different matrix (calls %d)", calls.Load())
	}
}

func TestGeometryReturnsThePathOrNothing(t *testing.T) {
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/foot-walking/geojson") {
			t.Errorf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"features":[{"geometry":{"type":"LineString","coordinates":[[-9.1334,38.7139],[-9.136,38.718],[-9.1393,38.7223]]}}]}`)
	})
	c := New(Config{Key: "k", DirectionsURL: s.URL + "/v2/directions"})

	line, err := c.Geometry(context.Background(), Walking, pts[:2])
	if err != nil || len(line) != 3 || line[0] != (Point{Lat: 38.7139, Lon: -9.1334}) {
		t.Fatalf("line = %v, %v", line, err)
	}

	keyless := New(Config{DirectionsURL: s.URL})
	if line, err := keyless.Geometry(context.Background(), Walking, pts[:2]); line != nil || err != nil {
		t.Errorf("without a key there is no geometry and no error, got %v, %v", line, err)
	}
}

func TestTooManyPointsAreEstimatedRatherThanSent(t *testing.T) {
	var calls atomic.Int32
	s := serve(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	c := New(Config{Key: "k", MatrixURL: s.URL})
	big := make([]Point, 60)
	for i := range big {
		big[i] = Point{Lat: 38 + float64(i)*0.001, Lon: -9}
	}
	m, err := c.Matrix(context.Background(), Walking, big)
	if err != nil || !m.Estimated || calls.Load() != 0 || len(m.Durations) != 60 {
		t.Errorf("estimated=%v err=%v calls=%d", m.Estimated, err, calls.Load())
	}
}
