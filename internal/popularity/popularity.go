// Package popularity estimates how well known a place is, from free public signals:
// how many Wikipedias have an article about it (Wikidata sitelinks) and how many people
// read the English article (Wikimedia pageviews). The numbers feed the candidate
// ranking, where they become a human-readable reason ("covered in 41 languages").
package popularity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultWikidataURL  = "https://www.wikidata.org/w/api.php"
	DefaultPageviewsURL = "https://wikimedia.org/api/rest_v1/metrics/pageviews/per-article/en.wikipedia/all-access/user"
	DefaultUserAgent    = "Trequila/1.0 (https://github.com/sky-walkerx/trequila)"

	// CacheTTL: popularity moves slowly.
	CacheTTL = 30 * 24 * time.Hour

	batchSize       = 50 // wbgetentities accepts at most 50 ids per request
	pageviewWorkers = 8
	maxBody         = 8 << 20
)

// Signals are the popularity inputs for one place. The zero value means "nothing known".
type Signals struct {
	Sitelinks int `json:"sitelinks"` // Wikipedia editions with an article
	Pageviews int `json:"pageviews"` // English Wikipedia views over the last twelve complete months
}

// Cache remembers results between calls; failures behave like misses.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
}

// Config configures a Client. The zero value is usable.
type Config struct {
	WikidataURL  string // empty = DefaultWikidataURL
	PageviewsURL string // empty = DefaultPageviewsURL (the per-article base, without the title)
	UserAgent    string
	Cache        Cache
	HTTP         *http.Client
	Now          func() time.Time
	Log          *slog.Logger
}

// Client looks up popularity signals.
type Client struct {
	wikidataURL  string
	pageviewsURL string
	userAgent    string
	cache        Cache
	http         *http.Client
	now          func() time.Time
	log          *slog.Logger
}

// New builds a Client.
func New(cfg Config) *Client {
	c := &Client{
		wikidataURL:  cfg.WikidataURL,
		pageviewsURL: strings.TrimRight(cfg.PageviewsURL, "/"),
		userAgent:    cfg.UserAgent,
		cache:        cfg.Cache,
		http:         cfg.HTTP,
		now:          cfg.Now,
		log:          cfg.Log,
	}
	if c.wikidataURL == "" {
		c.wikidataURL = DefaultWikidataURL
	}
	if c.pageviewsURL == "" {
		c.pageviewsURL = DefaultPageviewsURL
	}
	if c.userAgent == "" {
		c.userAgent = DefaultUserAgent
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 20 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c
}

var wikidataID = regexp.MustCompile(`^Q[1-9][0-9]*$`)

// Score returns the signals for every valid Wikidata ID given. The map always has an
// entry for each valid ID: a place with no data, or whose lookup failed, is zero, so a
// flaky upstream can never fail a whole trip. The error, when not nil, joins whatever
// failed so the caller can log it; zeros produced by a failure are not cached.
func (c *Client) Score(ctx context.Context, ids []string) (map[string]Signals, error) {
	out := map[string]Signals{}
	var todo []string
	for _, id := range ids {
		if !wikidataID.MatchString(id) {
			continue
		}
		if _, dup := out[id]; dup {
			continue
		}
		if s, ok := c.cached(ctx, id); ok {
			out[id] = s
			continue
		}
		out[id] = Signals{}
		todo = append(todo, id)
	}
	sort.Strings(todo) // stable batches make behaviour and tests reproducible

	var errs []error
	for start := 0; start < len(todo); start += batchSize {
		end := min(start+batchSize, len(todo))
		batch := todo[start:end]
		sites, err := c.sitelinks(ctx, batch)
		if err != nil {
			errs = append(errs, err)
			continue // zeros stay, uncached
		}
		for id, s := range c.withPageviews(ctx, batch, sites, &errs) {
			out[id] = s
			c.store(ctx, id, s)
		}
	}
	return out, errors.Join(errs...)
}

func cacheKey(id string) string { return "popularity:v1:" + id }

func (c *Client) cached(ctx context.Context, id string) (Signals, bool) {
	if c.cache == nil {
		return Signals{}, false
	}
	raw, ok := c.cache.Get(ctx, cacheKey(id))
	if !ok {
		return Signals{}, false
	}
	var s Signals
	if json.Unmarshal(raw, &s) != nil {
		return Signals{}, false
	}
	return s, true
}

func (c *Client) store(ctx context.Context, id string, s Signals) {
	if c.cache == nil {
		return
	}
	if raw, err := json.Marshal(s); err == nil {
		c.cache.Set(ctx, cacheKey(id), raw, CacheTTL)
	}
}

// entity is what is kept of a Wikidata item.
type entity struct {
	sitelinks int
	enTitle   string
}

// notWikipedia are sitelink sites whose key ends in "wiki" but which are not Wikipedias.
var notWikipedia = map[string]bool{
	"commonswiki": true, "wikidatawiki": true, "specieswiki": true, "metawiki": true,
	"mediawikiwiki": true, "sourceswiki": true, "foundationwiki": true, "incubatorwiki": true,
	"outreachwiki": true, "wikimaniawiki": true, "testwiki": true, "test2wiki": true,
}

func (c *Client) sitelinks(ctx context.Context, ids []string) (map[string]entity, error) {
	q := url.Values{
		"action": {"wbgetentities"}, "ids": {strings.Join(ids, "|")},
		"props": {"sitelinks"}, "format": {"json"},
	}
	var doc struct {
		Entities map[string]struct {
			Missing   *string `json:"missing"`
			Sitelinks map[string]struct {
				Title string `json:"title"`
			} `json:"sitelinks"`
		} `json:"entities"`
	}
	if err := c.getJSON(ctx, c.wikidataURL+"?"+q.Encode(), &doc); err != nil {
		return nil, fmt.Errorf("popularity: wikidata: %w", err)
	}
	out := make(map[string]entity, len(ids))
	for id, e := range doc.Entities {
		if e.Missing != nil {
			continue
		}
		var ent entity
		for site, l := range e.Sitelinks {
			if strings.HasSuffix(site, "wiki") && !notWikipedia[site] {
				ent.sitelinks++
			}
			if site == "enwiki" {
				ent.enTitle = l.Title
			}
		}
		out[id] = ent
	}
	return out, nil
}

// withPageviews adds English pageviews to the batch's entities, a few at a time.
func (c *Client) withPageviews(ctx context.Context, batch []string, sites map[string]entity, errs *[]error) map[string]Signals {
	out := make(map[string]Signals, len(batch))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, pageviewWorkers)
	for _, id := range batch {
		ent := sites[id] // a missing entity is the zero entity
		s := Signals{Sitelinks: ent.sitelinks}
		if ent.enTitle == "" {
			mu.Lock()
			out[id] = s
			mu.Unlock()
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			views, err := c.pageviews(ctx, ent.enTitle)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// The sitelink count is still good; only the views are missing. Not caching
				// this entry would make every retry repeat the sitelink call too, so it is
				// cached with zero views and expires with the rest.
				*errs = append(*errs, err)
			}
			s.Pageviews = views
			out[id] = s
		}()
	}
	wg.Wait()
	return out
}

// pageviews sums the English article's views over the last twelve complete months.
// An article with no recorded views answers 404, which is zero, not a failure.
func (c *Client) pageviews(ctx context.Context, title string) (int, error) {
	now := c.now().UTC()
	firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	start := firstOfThisMonth.AddDate(-1, 0, 0)
	end := firstOfThisMonth.AddDate(0, 0, -1)
	u := fmt.Sprintf("%s/%s/monthly/%s00/%s00", c.pageviewsURL, escapeTitle(title), start.Format("20060102"), end.Format("20060102"))

	var doc struct {
		Items []struct {
			Views int `json:"views"`
		} `json:"items"`
	}
	err := c.getJSON(ctx, u, &doc)
	var nf notFoundError
	if errors.As(err, &nf) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("popularity: pageviews for %q: %w", title, err)
	}
	total := 0
	for _, it := range doc.Items {
		total += it.Views
	}
	return total, nil
}

// escapeTitle turns an article title into a path segment: spaces become underscores and
// everything outside the unreserved set is percent-encoded, as the API requires ("/" too).
func escapeTitle(title string) string {
	title = strings.ReplaceAll(strings.TrimSpace(title), " ", "_")
	var b strings.Builder
	for i := 0; i < len(title); i++ {
		ch := title[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

type notFoundError struct{}

func (notFoundError) Error() string { return "not found" }

func (c *Client) getJSON(ctx context.Context, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return notFoundError{}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
