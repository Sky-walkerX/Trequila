package popularity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeWiki struct {
	t          *testing.T
	entities   map[string]map[string]string // id -> site -> title
	views      map[string]int               // en title (underscored) -> total views over the year
	wbCalls    atomic.Int32
	pvCalls    atomic.Int32
	batchSizes []int
	mu         sync.Mutex
	failIDs    bool
	userAgents []string
	lastPV     string
}

func (f *fakeWiki) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/wikidata/w/api.php", func(w http.ResponseWriter, r *http.Request) {
		f.wbCalls.Add(1)
		f.mu.Lock()
		f.userAgents = append(f.userAgents, r.Header.Get("User-Agent"))
		f.mu.Unlock()
		if f.failIDs {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		ids := strings.Split(r.URL.Query().Get("ids"), "|")
		f.mu.Lock()
		f.batchSizes = append(f.batchSizes, len(ids))
		f.mu.Unlock()
		out := map[string]any{}
		for _, id := range ids {
			sites, ok := f.entities[id]
			if !ok {
				out[id] = map[string]any{"id": id, "missing": ""}
				continue
			}
			links := map[string]any{}
			for site, title := range sites {
				links[site] = map[string]any{"site": site, "title": title}
			}
			out[id] = map[string]any{"id": id, "sitelinks": links}
		}
		json.NewEncoder(w).Encode(map[string]any{"entities": out})
	})
	mux.HandleFunc("/pv/", func(w http.ResponseWriter, r *http.Request) {
		f.pvCalls.Add(1)
		f.mu.Lock()
		f.lastPV = r.URL.EscapedPath()
		f.mu.Unlock()
		// /pv/en.wikipedia/all-access/user/<title>/monthly/<start>/<end>
		parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/pv/"), "/")
		title, _ := url.PathUnescape(parts[3])
		v, ok := f.views[title]
		if !ok {
			http.Error(w, `{"type":"not_found"}`, http.StatusNotFound)
			return
		}
		// Spread the total over 12 months.
		items := make([]map[string]any, 12)
		for i := range items {
			items[i] = map[string]any{"views": v / 12}
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	s := httptest.NewServer(mux)
	f.t.Cleanup(s.Close)
	return s
}

func newClient(t *testing.T, f *fakeWiki, cfg Config) *Client {
	s := f.server()
	cfg.WikidataURL = s.URL + "/wikidata/w/api.php"
	cfg.PageviewsURL = s.URL + "/pv/en.wikipedia/all-access/user"
	cfg.Now = func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
	return New(cfg)
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

func TestScoreCombinesSitelinksAndPageviews(t *testing.T) {
	f := &fakeWiki{t: t,
		entities: map[string]map[string]string{
			"Q1": {"enwiki": "Belém Tower", "frwiki": "Tour de Belém", "ptwiki": "Torre de Belém", "commonswiki": "Category:Belém Tower", "wikidatawiki": "x"},
			"Q2": {"ptwiki": "Só em português"},
			"Q3": {},
		},
		views: map[string]int{"Belém_Tower": 120000},
	}
	c := newClient(t, f, Config{UserAgent: "Trequila-test/1.0"})

	got, err := c.Score(context.Background(), []string{"Q1", "Q2", "Q3", "Q404"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Signals{
		"Q1":   {Sitelinks: 3, Pageviews: 120000}, // commons and wikidata are not Wikipedias
		"Q2":   {Sitelinks: 1},                    // no English article: no pageviews
		"Q3":   {},
		"Q404": {}, // a missing entity degrades to zero, it is not an error
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s = %+v, want %+v", id, got[id], w)
		}
	}
	if f.userAgents[0] != "Trequila-test/1.0" {
		t.Errorf("User-Agent = %q", f.userAgents[0])
	}
}

func TestPageviewsAskForTheLastTwelveCompleteMonthsAndEscapeTheTitle(t *testing.T) {
	f := &fakeWiki{t: t,
		entities: map[string]map[string]string{"Q1": {"enwiki": "AC/DC Lane (city)"}},
		views:    map[string]int{"AC/DC_Lane_(city)": 1200},
	}
	c := newClient(t, f, Config{})

	got, err := c.Score(context.Background(), []string{"Q1"})
	if err != nil || got["Q1"].Pageviews != 1200 {
		t.Fatalf("got %+v, %v (path %q)", got["Q1"], err, f.lastPV)
	}
	if !strings.Contains(f.lastPV, "AC%2FDC_Lane_%28city%29") {
		t.Errorf("title not fully percent-encoded: %q", f.lastPV)
	}
	if !strings.HasSuffix(f.lastPV, "/monthly/2025100100/2026093000") {
		t.Errorf("range = %q, want the twelve complete months before October 2026", f.lastPV)
	}
}

func TestMoreThanFiftyIDsAreSplitIntoBatches(t *testing.T) {
	f := &fakeWiki{t: t, entities: map[string]map[string]string{}}
	var ids []string
	for i := 1; i <= 120; i++ {
		id := fmt.Sprintf("Q%d", i)
		ids = append(ids, id)
		f.entities[id] = map[string]string{"ptwiki": "x"}
	}
	c := newClient(t, f, Config{})

	got, err := c.Score(context.Background(), append(ids, "Q1", "Q2")) // duplicates are asked once
	if err != nil || len(got) != 120 {
		t.Fatalf("got %d results, %v", len(got), err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batchSizes) != 3 {
		t.Fatalf("batches = %v, want 3 (50+50+20)", f.batchSizes)
	}
	for _, n := range f.batchSizes {
		if n > 50 {
			t.Errorf("a batch of %d exceeds the API limit of 50", n)
		}
	}
}

func TestMalformedIDsAreIgnored(t *testing.T) {
	f := &fakeWiki{t: t, entities: map[string]map[string]string{"Q1": {"ptwiki": "x"}}}
	c := newClient(t, f, Config{})

	got, err := c.Score(context.Background(), []string{"Q1", "", "not-an-id", "Q1|Q2", "P31"})
	if err != nil || len(got) != 1 || f.wbCalls.Load() != 1 {
		t.Fatalf("got %v, %v after %d upstream calls; want only Q1 looked up", got, err, f.wbCalls.Load())
	}
}

func TestAFailedLookupDegradesToZeroAndIsReportedButNotCached(t *testing.T) {
	f := &fakeWiki{t: t, failIDs: true}
	cache := &memCache{}
	c := newClient(t, f, Config{Cache: cache})

	got, err := c.Score(context.Background(), []string{"Q1"})
	if err == nil {
		t.Error("the failure should be reported so the caller can log it")
	}
	if s, ok := got["Q1"]; !ok || s != (Signals{}) {
		t.Errorf("Q1 = %+v, %v; want a zero entry so callers need no nil checks", s, ok)
	}
	if len(cache.m) != 0 {
		t.Errorf("a failure was cached: %v", cache.m)
	}
}

func TestAPageviewFailureKeepsTheSitelinkCount(t *testing.T) {
	f := &fakeWiki{t: t,
		entities: map[string]map[string]string{"Q1": {"enwiki": "Missing Article", "dewiki": "x"}},
		views:    map[string]int{}, // pageviews answer 404
	}
	c := newClient(t, f, Config{})

	got, err := c.Score(context.Background(), []string{"Q1"})
	if err != nil || got["Q1"] != (Signals{Sitelinks: 2}) {
		t.Fatalf("got %+v, %v; a 404 for pageviews means zero views, not a failure", got["Q1"], err)
	}
}

func TestResultsAreCachedPerID(t *testing.T) {
	f := &fakeWiki{t: t,
		entities: map[string]map[string]string{"Q1": {"enwiki": "A", "dewiki": "A"}, "Q2": {"enwiki": "B"}},
		views:    map[string]int{"A": 1200, "B": 2400},
	}
	c := newClient(t, f, Config{Cache: &memCache{}})

	if _, err := c.Score(context.Background(), []string{"Q1"}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Score(context.Background(), []string{"Q1", "Q2"})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batchSizes) != 2 || f.batchSizes[1] != 1 {
		t.Errorf("batches = %v; the second call should ask only for the uncached Q2", f.batchSizes)
	}
	if got["Q1"].Sitelinks != 2 || got["Q2"].Pageviews != 2400 {
		t.Errorf("got %+v", got)
	}
}
