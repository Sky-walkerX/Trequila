package osm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sampleResponse = `{
  "version": 0.6,
  "elements": [
    {"type":"node","id":101,"lat":38.6916,"lon":-9.2160,
     "tags":{"name":"Belém Tower","tourism":"attraction","opening_hours":"Tu-Su 10:00-18:30","wikidata":"Q193396","wikipedia":"en:Belém Tower","fee":"yes"}},
    {"type":"way","id":202,"center":{"lat":38.7139,"lon":-9.1334},
     "tags":{"name":"Praça do Comércio","historic":"yes","website":"https://example.org"}},
    {"type":"node","id":303,"lat":38.7100,"lon":-9.1400,
     "tags":{"name":"Café A Brasileira","amenity":"cafe"}},
    {"type":"node","id":404,"lat":38.7,"lon":-9.1,"tags":{"amenity":"cafe"}}
  ]
}`

func newClient(t *testing.T, cfg Config, urls ...string) *Client {
	t.Helper()
	cfg.URLs = urls
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = time.Millisecond
	}
	return New(cfg)
}

func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, sampleResponse)
}

func TestAroundParsesNodesAndWaysAndDropsUnnamedOnes(t *testing.T) {
	c := newClient(t, Config{}, serve(t, ok))

	got, err := c.Around(context.Background(), 38.7, -9.14, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d elements, want 3 (the unnamed cafe is dropped)", len(got))
	}
	tower, square := got[0], got[1]
	if tower.Ref() != "node/101" || tower.Name != "Belém Tower" || tower.Lat != 38.6916 || tower.Lon != -9.2160 {
		t.Errorf("node = %+v", tower)
	}
	if square.Ref() != "way/202" || square.Lat != 38.7139 || square.Lon != -9.1334 {
		t.Errorf("a way must take its centre as its position: %+v", square)
	}
}

func TestElementHelpers(t *testing.T) {
	cases := []struct {
		name     string
		tags     map[string]string
		kind     Kind
		category string
		setting  Setting
		free     bool
	}{
		{"museum", map[string]string{"tourism": "museum"}, KindSight, "museum", SettingIndoor, false},
		{"park", map[string]string{"leisure": "park", "fee": "no"}, KindSight, "park", SettingOutdoor, true},
		{"castle", map[string]string{"historic": "castle"}, KindSight, "castle", SettingUnknown, false},
		{"restaurant", map[string]string{"amenity": "restaurant"}, KindMeal, "restaurant", SettingUnknown, false},
		{"viewpoint", map[string]string{"tourism": "viewpoint"}, KindSight, "viewpoint", SettingOutdoor, false},
		{"explicit indoor", map[string]string{"tourism": "attraction", "indoor": "yes"}, KindSight, "attraction", SettingIndoor, false},
		{"beach", map[string]string{"natural": "beach"}, KindSight, "beach", SettingOutdoor, false},
		{"unrelated", map[string]string{"shop": "bakery"}, KindNone, "", SettingUnknown, false},
	}
	for _, tc := range cases {
		e := Element{Name: tc.name, Tags: tc.tags}
		if e.Kind() != tc.kind || e.Category() != tc.category || e.Setting() != tc.setting || e.Free() != tc.free {
			t.Errorf("%s: kind=%v category=%q setting=%v free=%v; want %v %q %v %v",
				tc.name, e.Kind(), e.Category(), e.Setting(), e.Free(), tc.kind, tc.category, tc.setting, tc.free)
		}
	}

	e := Element{Tags: map[string]string{"wikidata": "Q193396", "wikipedia": "pt:Torre de Belém", "contact:website": "https://a.example"}}
	if e.Wikidata() != "Q193396" {
		t.Errorf("Wikidata = %q", e.Wikidata())
	}
	if lang, title := e.Wikipedia(); lang != "pt" || title != "Torre de Belém" {
		t.Errorf("Wikipedia = %q, %q", lang, title)
	}
	if e.Website() != "https://a.example" {
		t.Errorf("Website falls back to contact:website, got %q", e.Website())
	}
	if lang, title := (Element{Tags: map[string]string{"wikipedia": "Bare title"}}).Wikipedia(); lang != "" || title != "" {
		t.Errorf("a wikipedia tag without a language prefix is not usable: %q, %q", lang, title)
	}
	if got := (Element{Tags: map[string]string{"wikidata": "not-an-id"}}).Wikidata(); got != "" {
		t.Errorf("a malformed wikidata id must be ignored, got %q", got)
	}
}

func TestTheQueryAsksForCentresWithinTheRadiusAndIdentifiesItself(t *testing.T) {
	var body, ua string
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		body, ua = r.PostForm.Get("data"), r.Header.Get("User-Agent")
		ok(w, r)
	})
	c := newClient(t, Config{UserAgent: "Trequila-test/1.0"}, url)

	if _, err := c.Around(context.Background(), 38.7, -9.14, 5000); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"around:5000,38.700000,-9.140000", ")->.sights;", ")->.meals;", ".sights out center tags", ".meals out center tags", `"tourism"`, `"amenity"`, `"historic"`} {
		if !strings.Contains(body, want) {
			t.Errorf("query is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "tourism\"~\"^(hotel") || strings.Contains(body, "hostel") {
		t.Errorf("accommodation must not be requested:\n%s", body)
	}
	if ua != "Trequila-test/1.0" {
		t.Errorf("User-Agent = %q", ua)
	}
}

func TestAnHTMLErrorPageIsAnErrorNeverAnEmptyResult(t *testing.T) {
	for name, status := range map[string]int{"overloaded": http.StatusGatewayTimeout, "wrong content": http.StatusOK} {
		url := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, "<html><body>The server is probably too busy</body></html>")
		})
		c := newClient(t, Config{}, url)
		got, err := c.Around(context.Background(), 38.7, -9.14, 5000)
		if err == nil || got != nil {
			t.Errorf("%s: got %v, %v; want an error and no elements", name, got, err)
		}
	}
}

func TestATransientStatusIsRetriedOnTheSameMirror(t *testing.T) {
	var calls atomic.Int32
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "slot", http.StatusTooManyRequests)
			return
		}
		ok(w, r)
	})
	c := newClient(t, Config{}, url)

	got, err := c.Around(context.Background(), 38.7, -9.14, 5000)
	if err != nil || len(got) != 3 || calls.Load() != 2 {
		t.Fatalf("got %d elements, %v after %d calls; want success on the second", len(got), err, calls.Load())
	}
}

func TestAMirrorThatKeepsFailingFailsOverToTheNext(t *testing.T) {
	var dead atomic.Int32
	down := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		dead.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	up := serve(t, ok)
	c := newClient(t, Config{}, down, up)

	got, err := c.Around(context.Background(), 38.7, -9.14, 5000)
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d, %v; want the second mirror's answer", len(got), err)
	}
	if dead.Load() != 2 {
		t.Errorf("the failing mirror was tried %d times, want 2 (one retry)", dead.Load())
	}
}

func TestAClientErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	url := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "bad query", http.StatusBadRequest)
	})
	c := newClient(t, Config{}, url)

	if _, err := c.Around(context.Background(), 38.7, -9.14, 5000); err == nil {
		t.Fatal("want an error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1: retrying a malformed query cannot help", calls.Load())
	}
}

type mapCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (c *mapCache) Get(_ context.Context, k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *mapCache) Set(_ context.Context, k string, v []byte, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string][]byte{}
	}
	c.m[k] = v
}

func TestACachedAreaDoesNotHitOverpassAgain(t *testing.T) {
	var calls atomic.Int32
	url := serve(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); ok(w, r) })
	c := newClient(t, Config{Cache: &mapCache{}}, url)

	first, err := c.Around(context.Background(), 38.7001, -9.1401, 5000)
	if err != nil {
		t.Fatal(err)
	}
	// A few metres away is the same cell.
	second, err := c.Around(context.Background(), 38.7003, -9.1399, 5000)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("second call: %v, upstream calls = %d, want 1", err, calls.Load())
	}
	if len(first) != len(second) || second[0].Ref() != "node/101" {
		t.Errorf("cached result differs: %d vs %d elements", len(first), len(second))
	}
	if _, err := c.Around(context.Background(), 38.7001, -9.1401, 8000); err != nil || calls.Load() != 2 {
		t.Errorf("a different radius must not share a cache entry (calls = %d, %v)", calls.Load(), err)
	}
}

func TestAnErrorIsNeverCached(t *testing.T) {
	var calls atomic.Int32
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 { // both attempts of the first Around fail
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		ok(w, r)
	})
	c := newClient(t, Config{Cache: &mapCache{}}, url)

	if _, err := c.Around(context.Background(), 38.7, -9.14, 5000); err == nil {
		t.Fatal("want the first call to fail")
	}
	if got, err := c.Around(context.Background(), 38.7, -9.14, 5000); err != nil || len(got) != 3 {
		t.Fatalf("second call = %d elements, %v; a failure must not poison the cache", len(got), err)
	}
}

func TestConcurrentQueriesAreCapped(t *testing.T) {
	var inFlight, peak atomic.Int32
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		inFlight.Add(-1)
		ok(w, r)
	})
	c := newClient(t, Config{MaxConcurrent: 2}, url)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.Around(context.Background(), 38.0+float64(i), -9.0, 5000); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Errorf("peak concurrency = %d, want at most 2", peak.Load())
	}
}

func TestAWaitingCallerGivesUpWhenItsContextEnds(t *testing.T) {
	release := make(chan struct{})
	url := serve(t, func(w http.ResponseWriter, r *http.Request) { <-release; ok(w, r) })
	defer close(release)
	c := newClient(t, Config{MaxConcurrent: 1}, url)

	go c.Around(context.Background(), 1, 1, 5000) // takes the only slot
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Around(ctx, 2, 2, 5000); err == nil {
		t.Fatal("want the queued call to fail when its context expires")
	}
}
