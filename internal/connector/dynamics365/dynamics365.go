// Package dynamics365 reads a winery web store built on Microsoft Dynamics
// 365 Commerce (nagus-390).
//
// Chateau Ste Michelle (Woodinville, WA) runs its store on D365 Commerce (its
// sitemaps live under /_msdyn365/). There is no public JSON feed, but every
// category page embeds the storefront's own list state in
// window.___initialData___:
//
//	"LISTPAGESTATE": {... "result": {"totalProductCount": 132, "pageSize": 50,
//	    "activeProducts": [{"ItemId", "Name", "Price", "BasePrice", "RecordId",
//	        "AttributeValues": [{"Name": "Vintage", "TextValue": "2024"}, ...]}]}}
//
// so this decodes exactly that array. BasePrice is the list price; Price the
// selling price. Pages advance with ?skip=N. The store's robots.txt asks for a
// ten-second crawl delay, which is the default pause between pages. Products
// whose Product Type is "Assembly" are multi-bottle sets and are skipped, as
// the Commerce7 connector skips bundles: a per-bottle deal needs one bottle.
package dynamics365

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/connector/webfetch"
	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the connector family; a configured source is "dynamics365:<Name>".
const SourceID = "dynamics365"

// DefaultPause honors the ten-second Crawl-Delay Ste Michelle's robots.txt sets.
const DefaultPause = 10 * time.Second

// DefaultMaxPages bounds one fetch (50 products per page).
const DefaultMaxPages = 10

// Config configures one store.
type Config struct {
	Name string
	// StoreURL is the site root, e.g. "https://www.ste-michelle.com".
	StoreURL string
	// CatalogPath is the category page, e.g.
	// "/chateau-ste-michelle/shop/all-wines/5637155140.c".
	CatalogPath string
	MaxPages    int
	FixturePath string
	Fetch       *webfetch.Client
	Pause       time.Duration
	Sleep       func(context.Context, time.Duration) error
	Now         func() time.Time
	Logf        func(string, ...any)
}

// Connector implements listing.Connector.
type Connector struct {
	cfg          Config
	mu           sync.Mutex
	lastComplete bool
}

// NewConnector fills defaults. A zero Pause means DefaultPause; pass a
// negative Pause for none (tests).
func NewConnector(cfg Config) *Connector {
	cfg.StoreURL = strings.TrimRight(cfg.StoreURL, "/")
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = DefaultMaxPages
	}
	if cfg.Pause == 0 {
		cfg.Pause = DefaultPause
	}
	if cfg.Fetch == nil {
		cfg.Fetch = &webfetch.Client{}
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Connector{cfg: cfg}
}

// SourceID returns "dynamics365:<Name>".
func (c *Connector) SourceID() string { return SourceID + ":" + c.cfg.Name }

// FetchComplete reports whether the last Fetch read every page.
func (c *Connector) FetchComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastComplete
}

type attribute struct {
	Name       string   `json:"Name"`
	TextValue  string   `json:"TextValue"`
	FloatValue *float64 `json:"FloatValue"`
}

type product struct {
	ItemID          string      `json:"ItemId"`
	Name            string      `json:"Name"`
	Price           float64     `json:"Price"`
	BasePrice       float64     `json:"BasePrice"`
	RecordID        int64       `json:"RecordId"`
	AttributeValues []attribute `json:"AttributeValues"`
}

type listState struct {
	TotalProductCount int       `json:"totalProductCount"`
	ActiveProducts    []product `json:"activeProducts"`
}

var reProductHref = regexp.MustCompile(`href="([^"]+/(\d+)\.p)"`)

// Fetch reads the category pages and returns one Raw per single-bottle product.
func (c *Connector) Fetch(ctx context.Context) ([]listing.Raw, error) {
	c.setComplete(false)
	if c.cfg.FixturePath == "" && (c.cfg.StoreURL == "" || c.cfg.CatalogPath == "") {
		return nil, errors.New("dynamics365: storeUrl and catalogPath are required")
	}
	now := c.cfg.Now()
	skipped := map[string]int{}
	var out []listing.Raw
	seen := 0
	for page := 0; page < c.cfg.MaxPages; page++ {
		if page > 0 && c.cfg.Pause > 0 {
			if err := c.cfg.Sleep(ctx, c.cfg.Pause); err != nil {
				return nil, err
			}
		}
		body, err := c.page(ctx, page*50)
		if err != nil {
			return nil, fmt.Errorf("dynamics365 %s: %w", c.cfg.Name, err)
		}
		st, links, err := parse(body)
		if err != nil {
			return nil, fmt.Errorf("dynamics365 %s page %d: %w", c.cfg.Name, page, err)
		}
		for _, p := range st.ActiveProducts {
			r, why := c.raw(p, links, now)
			if why != "" {
				skipped[why]++
				continue
			}
			out = append(out, r)
		}
		seen += len(st.ActiveProducts)
		if len(st.ActiveProducts) == 0 || seen >= st.TotalProductCount || c.cfg.FixturePath != "" {
			if c.cfg.Logf != nil {
				c.cfg.Logf("dynamics365 %s: %d wine listings of %d products, skipped %v", c.cfg.Name, len(out), seen, skipped)
			}
			c.setComplete(true)
			return out, nil
		}
	}
	// Ran out of pages before the store's own count: return what was read but
	// do not claim a complete fetch (items missing from it must not be purged).
	if c.cfg.Logf != nil {
		c.cfg.Logf("dynamics365 %s: stopped at %d pages with %d products read; raise maxPages", c.cfg.Name, c.cfg.MaxPages, seen)
	}
	return out, nil
}

func (c *Connector) page(ctx context.Context, skip int) ([]byte, error) {
	if c.cfg.FixturePath != "" {
		return os.ReadFile(c.cfg.FixturePath)
	}
	u := c.cfg.StoreURL + c.cfg.CatalogPath
	if skip > 0 {
		u += "?skip=" + strconv.Itoa(skip)
	}
	return c.cfg.Fetch.Get(ctx, u, nil)
}

// parse finds the list state the storefront embeds and the product links.
func parse(body []byte) (listState, map[int64]string, error) {
	var st listState
	s := string(body)
	i := strings.Index(s, `"LISTPAGESTATE"`)
	if i < 0 {
		return st, nil, errors.New("no LISTPAGESTATE on the category page (store redesigned?)")
	}
	rest := s[i:]
	j := strings.Index(rest, `"result":`)
	if j < 0 {
		return st, nil, errors.New("LISTPAGESTATE has no result")
	}
	if err := json.NewDecoder(strings.NewReader(rest[j+len(`"result":`):])).Decode(&st); err != nil {
		return st, nil, fmt.Errorf("decode LISTPAGESTATE: %w", err)
	}
	links := map[int64]string{}
	for _, m := range reProductHref.FindAllStringSubmatch(s, -1) {
		if id, err := strconv.ParseInt(m[2], 10, 64); err == nil {
			links[id] = m[1]
		}
	}
	return st, links, nil
}

// raw maps one product; a non-empty reason means it was skipped.
func (c *Connector) raw(p product, links map[int64]string, now time.Time) (listing.Raw, string) {
	attr := map[string]string{}
	for _, a := range p.AttributeValues {
		if v := strings.TrimSpace(a.TextValue); v != "" {
			attr[a.Name] = v
		}
	}
	if attr["Product Type"] == "Assembly" {
		return listing.Raw{}, "multi-bottle set"
	}
	price := toCents(p.Price)
	if price <= 0 {
		return listing.Raw{}, "no price"
	}
	aspects := map[string]string{}
	if list := toCents(p.BasePrice); list > price {
		aspects["compare_at_cents"] = strconv.FormatInt(list, 10)
	}
	if v := attr["Vintage"]; v != "" {
		aspects["vintage"] = v
	}
	if v := attr["SLR Varietal"]; v != "" && !strings.EqualFold(v, "Blend") && !strings.EqualFold(v, "Red Blend") {
		aspects["varietal"] = v
	}
	if v := attr["Wine Color"]; v != "" {
		aspects["wine_type"] = v
	}
	if v := attr["SLR Appellation"]; v != "" {
		aspects["appellation"] = v
	}
	if ml := bottleML(attr["Wine Bottle Size"]); ml > 0 {
		aspects["bottle_ml"] = strconv.Itoa(ml)
	}
	link := links[p.RecordID]
	if link != "" && strings.HasPrefix(link, "/") {
		link = c.cfg.StoreURL + link
	}
	key := p.ItemID
	if key == "" {
		key = strconv.FormatInt(p.RecordID, 10)
	}
	return listing.Raw{
		SourceID:   c.SourceID(),
		SourceKey:  key,
		SourceURL:  link,
		Title:      strings.TrimSpace(p.Name),
		Body:       attr["Tasting Notes"],
		PriceCents: price,
		Currency:   "USD",
		Aspects:    aspects,
		SeenAt:     now,
	}, ""
}

var reML = regexp.MustCompile(`(?i)([\d.]+)\s*(ml|l)\b`)

// bottleML reads "750 ml" / "1.5 L".
func bottleML(s string) int {
	m := reML.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil || f <= 0 {
		return 0
	}
	if strings.EqualFold(m[2], "l") {
		f *= 1000
	}
	return int(math.Round(f))
}

func toCents(dollars float64) int64 {
	if dollars <= 0 {
		return 0
	}
	return int64(math.Round(dollars * 100))
}

func (c *Connector) setComplete(v bool) {
	c.mu.Lock()
	c.lastComplete = v
	c.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
