// Package imagery chooses photographs for a destination and for individual places, and
// always returns the attribution the licence requires. Photos come from Pexels when an
// operator key is configured and from Wikimedia Commons otherwise; the Commons results
// are filtered for size, orientation, file type, title and licence, because most of what
// a search returns is maps, logos, flags and portrait snapshots.
package imagery

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Defaults.
const (
	DefaultCommonsURL  = "https://commons.wikimedia.org/w/api.php"
	DefaultWikidataURL = "https://www.wikidata.org/w/api.php"
	DefaultPexelsURL   = "https://api.pexels.com/v1/search"
	DefaultUserAgent   = "Trequila/1.0 (https://github.com/sky-walkerx/trequila)"

	heroMinWidth    = 1600
	galleryMinWidth = 1200
	minAspect       = 1.3 // width / height: landscape, but...
	maxAspect       = 3.0 // ...not a panorama strip
	searchLimit     = 40
	maxBody         = 8 << 20
)

// Image is a photo with the facts needed to display and credit it.
type Image struct {
	URL         string `json:"url"`
	ThumbURL    string `json:"thumb_url,omitempty"`
	Source      string `json:"source"` // "wikimedia-commons" | "pexels"
	Attribution string `json:"attribution"`
	License     string `json:"license"`
	PageURL     string `json:"page_url"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// Config configures a Client. The zero value is usable and keyless.
type Config struct {
	CommonsURL  string
	WikidataURL string
	PexelsURL   string
	PexelsKey   string // optional; enables Pexels for hero images
	UserAgent   string
	HTTP        *http.Client
}

// Client finds images.
type Client struct {
	commonsURL, wikidataURL, pexelsURL, pexelsKey, userAgent string
	http                                                     *http.Client
}

// New builds a Client.
func New(cfg Config) *Client {
	c := &Client{commonsURL: cfg.CommonsURL, wikidataURL: cfg.WikidataURL, pexelsURL: cfg.PexelsURL,
		pexelsKey: cfg.PexelsKey, userAgent: cfg.UserAgent, http: cfg.HTTP}
	if c.commonsURL == "" {
		c.commonsURL = DefaultCommonsURL
	}
	if c.wikidataURL == "" {
		c.wikidataURL = DefaultWikidataURL
	}
	if c.pexelsURL == "" {
		c.pexelsURL = DefaultPexelsURL
	}
	if c.userAgent == "" {
		c.userAgent = DefaultUserAgent
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 20 * time.Second}
	}
	return c
}

// Hero returns the destination's main picture, or nil when nothing good enough exists.
// With a Pexels key it searches Pexels; otherwise it uses the destination's Wikidata
// image (cityWikidata may be empty) and then a Commons search.
func (c *Client) Hero(ctx context.Context, city, cityWikidata string) (*Image, error) {
	if c.pexelsKey != "" {
		img, err := c.pexelsHero(ctx, city)
		if err == nil && img != nil {
			return img, nil
		}
		// Fall through to Commons: an exhausted Pexels quota must not leave the trip bare.
	}
	if cityWikidata != "" {
		if img, err := c.PlacePhoto(ctx, cityWikidata); err == nil && img != nil && img.Width >= heroMinWidth {
			return img, nil
		}
	}
	imgs, err := c.commonsSearch(ctx, city, heroMinWidth, 1)
	if err != nil || len(imgs) == 0 {
		return nil, err
	}
	return &imgs[0], nil
}

// Gallery returns up to n further pictures of the destination.
func (c *Client) Gallery(ctx context.Context, city string, n int) ([]Image, error) {
	return c.commonsSearch(ctx, city, galleryMinWidth, n)
}

var wikidataID = regexp.MustCompile(`^Q[1-9][0-9]*$`)

// PlacePhoto returns the picture Wikidata names for an item (property P18), or nil.
func (c *Client) PlacePhoto(ctx context.Context, wikidata string) (*Image, error) {
	if !wikidataID.MatchString(wikidata) {
		return nil, nil
	}
	q := url.Values{"action": {"wbgetentities"}, "ids": {wikidata}, "props": {"claims"}, "format": {"json"}}
	var doc struct {
		Entities map[string]struct {
			Claims map[string][]struct {
				Mainsnak struct {
					Datavalue struct {
						Value json.RawMessage `json:"value"`
					} `json:"datavalue"`
				} `json:"mainsnak"`
			} `json:"claims"`
		} `json:"entities"`
	}
	if err := c.getJSON(ctx, c.wikidataURL+"?"+q.Encode(), nil, &doc); err != nil {
		return nil, fmt.Errorf("imagery: wikidata: %w", err)
	}
	claims := doc.Entities[wikidata].Claims["P18"]
	if len(claims) == 0 {
		return nil, nil
	}
	var file string
	if json.Unmarshal(claims[0].Mainsnak.Datavalue.Value, &file) != nil || file == "" {
		return nil, nil
	}
	imgs, err := c.commonsQuery(ctx, url.Values{"titles": {"File:" + file}}, 0, 1)
	if err != nil || len(imgs) == 0 {
		return nil, err
	}
	return &imgs[0], nil
}

// --- Commons ---

func (c *Client) commonsSearch(ctx context.Context, city string, minWidth, n int) ([]Image, error) {
	return c.commonsQuery(ctx, url.Values{
		"generator": {"search"}, "gsrsearch": {"filetype:bitmap " + city}, "gsrnamespace": {"6"}, "gsrlimit": {fmt.Sprint(searchLimit)},
	}, minWidth, n)
}

type commonsPage struct {
	Title     string `json:"title"`
	Index     int    `json:"index"`
	ImageInfo []struct {
		URL            string `json:"url"`
		ThumbURL       string `json:"thumburl"`
		Width          int    `json:"width"`
		Height         int    `json:"height"`
		Mime           string `json:"mime"`
		DescriptionURL string `json:"descriptionurl"`
		ExtMetadata    map[string]struct {
			Value json.RawMessage `json:"value"` // usually a string, sometimes a number
		} `json:"extmetadata"`
	} `json:"imageinfo"`
}

// commonsQuery runs a Commons query for image info and returns up to n usable images in
// search order. minWidth 0 skips the size test (an image Wikidata itself names).
func (c *Client) commonsQuery(ctx context.Context, params url.Values, minWidth, n int) ([]Image, error) {
	params.Set("action", "query")
	params.Set("format", "json")
	params.Set("prop", "imageinfo")
	params.Set("iiprop", "url|size|mime|extmetadata")
	params.Set("iiurlwidth", "800")
	var doc struct {
		Query struct {
			Pages map[string]commonsPage `json:"pages"`
		} `json:"query"`
	}
	if err := c.getJSON(ctx, c.commonsURL+"?"+params.Encode(), nil, &doc); err != nil {
		return nil, fmt.Errorf("imagery: commons: %w", err)
	}
	pages := make([]commonsPage, 0, len(doc.Query.Pages))
	for _, p := range doc.Query.Pages {
		pages = append(pages, p)
	}
	sort.Slice(pages, func(i, j int) bool {
		if pages[i].Index != pages[j].Index {
			return pages[i].Index < pages[j].Index
		}
		return pages[i].Title < pages[j].Title
	})

	var out []Image
	for _, p := range pages {
		if len(p.ImageInfo) == 0 || len(out) >= n {
			continue
		}
		ii := p.ImageInfo[0]
		if ii.Mime != "image/jpeg" && ii.Mime != "image/png" {
			continue
		}
		if badTitle(p.Title) || ii.Height <= 0 {
			continue
		}
		if minWidth > 0 {
			ratio := float64(ii.Width) / float64(ii.Height)
			if ii.Width < minWidth || ratio < minAspect || ratio > maxAspect {
				continue
			}
		}
		licence := metaString(ii.ExtMetadata["LicenseShortName"].Value)
		if !allowedLicence(licence) {
			continue
		}
		out = append(out, Image{
			URL: ii.URL, ThumbURL: ii.ThumbURL, Source: "wikimedia-commons",
			Attribution: stripHTML(metaString(ii.ExtMetadata["Artist"].Value)), License: licence,
			PageURL: ii.DescriptionURL, Width: ii.Width, Height: ii.Height,
		})
	}
	return out, nil
}

// metaString reads a metadata value that is a string, and treats anything else as empty.
func metaString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

var badTitleWords = regexp.MustCompile(`(?i)\b(map|maps|logo|flag|coat of arms|diagram|plan|locator|location|emblem|icon|seal|chart|poster|stamp)\b`)

func badTitle(t string) bool { return badTitleWords.MatchString(t) }

var ccLicence = regexp.MustCompile(`^CC BY(-SA)? [0-9.]+( .*)?$`)

// allowedLicence accepts the licences that let the photo be shown with credit: public
// domain, CC0, CC BY and CC BY-SA. Non-commercial and no-derivatives licences are out.
func allowedLicence(l string) bool {
	l = strings.TrimSpace(l)
	u := strings.ToUpper(l)
	switch {
	case strings.HasPrefix(u, "CC0"), strings.HasPrefix(u, "PUBLIC DOMAIN"), u == "PD" || strings.HasPrefix(u, "PD-"):
		return true
	}
	return ccLicence.MatchString(l) && !strings.Contains(u, "-NC") && !strings.Contains(u, "-ND")
}

var tags = regexp.MustCompile(`<[^>]*>`)

func stripHTML(s string) string {
	s = html.UnescapeString(tags.ReplaceAllString(s, ""))
	return strings.Join(strings.Fields(s), " ")
}

// --- Pexels ---

func (c *Client) pexelsHero(ctx context.Context, city string) (*Image, error) {
	q := url.Values{"query": {city}, "orientation": {"landscape"}, "size": {"large"}, "per_page": {"15"}}
	var doc struct {
		Photos []struct {
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			URL          string `json:"url"`
			Photographer string `json:"photographer"`
			Src          struct {
				Large2x string `json:"large2x"`
				Medium  string `json:"medium"`
			} `json:"src"`
		} `json:"photos"`
	}
	if err := c.getJSON(ctx, c.pexelsURL+"?"+q.Encode(), map[string]string{"Authorization": c.pexelsKey}, &doc); err != nil {
		return nil, fmt.Errorf("imagery: pexels: %w", err)
	}
	for _, p := range doc.Photos {
		if p.Height <= 0 || p.Width < heroMinWidth || p.Src.Large2x == "" {
			continue
		}
		if ratio := float64(p.Width) / float64(p.Height); ratio < minAspect || ratio > maxAspect {
			continue
		}
		return &Image{
			URL: p.Src.Large2x, ThumbURL: p.Src.Medium, Source: "pexels",
			Attribution: "Photo by " + p.Photographer + " on Pexels", License: "Pexels License",
			PageURL: p.URL, Width: p.Width, Height: p.Height,
		}, nil
	}
	return nil, nil
}

func (c *Client) getJSON(ctx context.Context, u string, headers map[string]string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
