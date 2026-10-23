package imagery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type page struct {
	title         string
	w, h          int
	mime, license string
	artist        string
	index         int
}

func fakePage(p page) map[string]any {
	return map[string]any{
		"title": p.title, "index": p.index,
		"imageinfo": []map[string]any{{
			"url": "https://upload.example/" + strings.ReplaceAll(p.title, " ", "_"), "thumburl": "https://thumb.example/" + strings.ReplaceAll(p.title, " ", "_"),
			"width": p.w, "height": p.h, "mime": p.mime,
			"descriptionurl": "https://commons.example/wiki/" + strings.ReplaceAll(p.title, " ", "_"),
			"extmetadata": map[string]any{
				"LicenseShortName":         map[string]any{"value": p.license},
				"Artist":                   map[string]any{"value": p.artist},
				"CommonsMetadataExtension": map[string]any{"value": 1.2}, // Commons mixes numbers in with the strings
			},
		}},
	}
}

type fake struct {
	t       *testing.T
	pages   []page            // returned by a Commons search
	byTitle map[string]page   // returned by a Commons titles lookup
	p18     map[string]string // wikidata id -> file name
	pexels  string            // JSON body for a Pexels search
	mu      sync.Mutex
	auth    string
	queries []string
}

func (f *fake) start() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/commons", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.queries = append(f.queries, q.Get("gsrsearch")+q.Get("titles"))
		f.mu.Unlock()
		pages := map[string]any{}
		if t := q.Get("titles"); t != "" {
			if p, ok := f.byTitle[t]; ok {
				pages["1"] = fakePage(p)
			}
		} else {
			for i, p := range f.pages {
				p.index = i + 1
				pages[fmt.Sprint(i+1)] = fakePage(p)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"pages": pages}})
	})
	mux.HandleFunc("/wikidata", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("ids")
		claims := map[string]any{}
		if name, ok := f.p18[id]; ok {
			claims["P18"] = []map[string]any{{"mainsnak": map[string]any{"datavalue": map[string]any{"value": name}}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"entities": map[string]any{id: map[string]any{"claims": claims}}})
	})
	mux.HandleFunc("/pexels", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = r.Header.Get("Authorization")
		f.mu.Unlock()
		w.Write([]byte(f.pexels))
	})
	s := httptest.NewServer(mux)
	f.t.Cleanup(s.Close)
	return s
}

func newClient(t *testing.T, f *fake, key string) *Client {
	s := f.start()
	return New(Config{
		CommonsURL: s.URL + "/commons", WikidataURL: s.URL + "/wikidata", PexelsURL: s.URL + "/pexels",
		PexelsKey: key, UserAgent: "test/1.0",
	})
}

var good = page{title: "File:Lisbon skyline.jpg", w: 4000, h: 2600, mime: "image/jpeg", license: "CC BY-SA 4.0", artist: `<a href="//commons.example/User:Jane">Jane Doe</a>`}

func TestGalleryKeepsOnlyUsableLandscapePhotosWithAllowedLicences(t *testing.T) {
	bad := func(mod func(*page)) page { p := good; mod(&p); return p }
	f := &fake{t: t, pages: []page{
		bad(func(p *page) { p.title = "File:Lisbon map.jpg" }),
		bad(func(p *page) { p.title = "File:Flag of Lisbon.png" }),
		bad(func(p *page) { p.title = "File:Lisbon vector.svg"; p.mime = "image/svg+xml" }),
		bad(func(p *page) { p.title = "File:Lisbon portrait.jpg"; p.w, p.h = 2000, 3000 }),
		bad(func(p *page) { p.title = "File:Lisbon small.jpg"; p.w, p.h = 800, 500 }),
		bad(func(p *page) { p.title = "File:Lisbon NC.jpg"; p.license = "CC BY-NC 4.0" }),
		bad(func(p *page) { p.title = "File:Lisbon ND.jpg"; p.license = "CC BY-ND 2.0" }),
		bad(func(p *page) { p.title = "File:Lisbon fair use.jpg"; p.license = "Fair use" }),
		bad(func(p *page) { p.title = "File:Lisbon strip.jpg"; p.w, p.h = 9000, 1500 }),
		bad(func(p *page) { p.title = "File:Lisbon tram.jpg"; p.license = "CC0" }),
		good,
		bad(func(p *page) { p.title = "File:Lisbon old.jpg"; p.license = "Public domain" }),
	}}
	c := newClient(t, f, "")

	got, err := c.Gallery(context.Background(), "Lisbon", 5)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, g := range got {
		titles = append(titles, strings.TrimPrefix(g.PageURL[strings.LastIndex(g.PageURL, "/")+1:], "File:"))
	}
	want := "Lisbon_tram.jpg,Lisbon_skyline.jpg,Lisbon_old.jpg"
	if strings.Join(titles, ",") != want {
		t.Errorf("gallery = %v, want %s (search order kept, unusable ones dropped)", titles, want)
	}
}

func TestEveryImageCarriesItsAttribution(t *testing.T) {
	f := &fake{t: t, pages: []page{good}}
	c := newClient(t, f, "")

	got, err := c.Gallery(context.Background(), "Lisbon", 3)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d, %v", len(got), err)
	}
	g := got[0]
	if g.Source != "wikimedia-commons" || g.License != "CC BY-SA 4.0" || g.Attribution != "Jane Doe" ||
		!strings.HasPrefix(g.PageURL, "https://commons.example/wiki/") || g.URL == "" || g.ThumbURL == "" || g.Width != 4000 || g.Height != 2600 {
		t.Errorf("image = %+v", g)
	}
}

func TestGalleryIsLimitedToN(t *testing.T) {
	f := &fake{t: t}
	for i := 0; i < 10; i++ {
		p := good
		p.title = fmt.Sprintf("File:Lisbon %d.jpg", i)
		f.pages = append(f.pages, p)
	}
	got, err := newClient(t, f, "").Gallery(context.Background(), "Lisbon", 4)
	if err != nil || len(got) != 4 {
		t.Errorf("got %d, %v; want 4", len(got), err)
	}
}

func TestHeroUsesPexelsWhenAKeyIsConfigured(t *testing.T) {
	f := &fake{t: t, pexels: `{"photos":[
		{"width":1000,"height":700,"url":"https://pexels.example/small","photographer":"Small","src":{"large2x":"https://img/small"}},
		{"width":4000,"height":2600,"url":"https://pexels.example/photo/1","photographer":"Ana Silva","photographer_url":"https://pexels.example/@ana",
		 "src":{"original":"https://img/orig","large2x":"https://img/large2x","landscape":"https://img/landscape"}}]}`}
	c := newClient(t, f, "secret-key")

	h, err := c.Hero(context.Background(), "Lisbon", "")
	if err != nil || h == nil {
		t.Fatalf("hero = %v, %v", h, err)
	}
	if h.Source != "pexels" || h.URL != "https://img/large2x" || h.Attribution != "Photo by Ana Silva on Pexels" || h.License != "Pexels License" || h.PageURL != "https://pexels.example/photo/1" {
		t.Errorf("hero = %+v", h)
	}
	if f.auth != "secret-key" {
		t.Errorf("Authorization = %q", f.auth)
	}
}

func TestHeroWithoutAKeyUsesTheCityPhotoOnWikidataThenSearch(t *testing.T) {
	p := good
	p.title = "File:Lisbon official.jpg"
	f := &fake{t: t,
		p18:     map[string]string{"Q597": "Lisbon official.jpg"},
		byTitle: map[string]page{"File:Lisbon official.jpg": p},
		pages:   []page{{title: "File:Other.jpg", w: 4000, h: 2600, mime: "image/jpeg", license: "CC0"}},
	}
	c := newClient(t, f, "")

	h, err := c.Hero(context.Background(), "Lisbon", "Q597")
	if err != nil || h == nil || !strings.Contains(h.PageURL, "Lisbon_official.jpg") {
		t.Fatalf("hero = %+v, %v; want the Wikidata P18 image", h, err)
	}

	h, err = c.Hero(context.Background(), "Lisbon", "Q9999") // no P18: fall back to a search
	if err != nil || h == nil || !strings.Contains(h.PageURL, "Other.jpg") {
		t.Fatalf("hero = %+v, %v; want the search result", h, err)
	}
}

func TestHeroNeedsAHigherResolutionThanTheGallery(t *testing.T) {
	p := good
	p.w, p.h = 1300, 850
	f := &fake{t: t, pages: []page{p}}
	c := newClient(t, f, "")

	if h, err := c.Hero(context.Background(), "Lisbon", ""); err != nil || h != nil {
		t.Errorf("hero = %+v, %v; a 1300 px image is gallery-grade only", h, err)
	}
	if g, _ := c.Gallery(context.Background(), "Lisbon", 3); len(g) != 1 {
		t.Errorf("the same image should still be gallery-grade, got %d", len(g))
	}
}

func TestPlacePhotoComesFromTheWikidataImage(t *testing.T) {
	p := good
	p.title = "File:Belem Tower.jpg"
	f := &fake{t: t, p18: map[string]string{"Q193396": "Belem Tower.jpg"}, byTitle: map[string]page{"File:Belem Tower.jpg": p}}
	c := newClient(t, f, "")

	img, err := c.PlacePhoto(context.Background(), "Q193396")
	if err != nil || img == nil || !strings.Contains(img.PageURL, "Belem_Tower.jpg") {
		t.Fatalf("photo = %+v, %v", img, err)
	}
	if none, err := c.PlacePhoto(context.Background(), "Q1"); err != nil || none != nil {
		t.Errorf("a place without an image gives nothing, got %+v, %v", none, err)
	}
	if none, err := c.PlacePhoto(context.Background(), "bogus"); err != nil || none != nil {
		t.Errorf("a malformed id gives nothing, got %+v, %v", none, err)
	}
}

func TestAnUpstreamFailureIsAnError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) }))
	defer s.Close()
	c := New(Config{CommonsURL: s.URL, WikidataURL: s.URL, PexelsURL: s.URL, HTTP: &http.Client{Timeout: time.Second}})
	if _, err := c.Gallery(context.Background(), "Lisbon", 3); err == nil {
		t.Error("want an error so the caller can log it; the caller decides to carry on without images")
	}
}
