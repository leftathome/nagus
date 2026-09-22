// Package commerce7 reads a winery's Commerce7 storefront catalogue
// (nagus-oc4).
//
// Commerce7 is the most common wine DTC platform nagus has met: 18 of 29 West
// Coast producers fingerprinted on 2026-09-21 (Tablas Creek, Ridge, L'Ecole,
// DeLille, K Vintners, Cristom, ...). Its admin API needs app credentials, but
// the storefront itself reads products from the PUBLIC route
//
//	GET https://api.commerce7.com/v1/product/for-web    (header  tenant: <slug>)
//
// with no credentials -- the same request every visitor's browser makes. This
// connector makes exactly that request and nothing more; routes that answer
// 401 (collections, customers, anything behind login) are never tried.
//
// Products carry structured wine data (type, varietal, appellation, region,
// vintage) and variants carry price, comparePrice (a store sale when above
// price), bottle size and availability. A product with no variants is a
// library/archive listing that cannot be bought and is skipped.
package commerce7

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/connector/webfetch"
	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the connector family; a configured source is "commerce7:<Name>".
const SourceID = "commerce7"

const (
	// DefaultAPIBase is the public storefront API.
	DefaultAPIBase = "https://api.commerce7.com/v1"
	// PageSize is products per request.
	PageSize = 50
	// DefaultMaxPages bounds one Fetch (50 x 20 = 1000 products).
	DefaultMaxPages = 20
)

// Config configures one winery.
type Config struct {
	// Name is the operator-chosen source name.
	Name string
	// Tenant is the Commerce7 tenant slug (`nagus fingerprint` finds it).
	Tenant string
	// StoreURL is the winery's storefront root, for product links
	// ("https://tablascreek.com" -> https://tablascreek.com/product/<slug>).
	StoreURL string
	// APIBase overrides DefaultAPIBase (tests).
	APIBase     string
	MaxPages    int
	FixturePath string
	Fetch       *webfetch.Client
	// Pause between page requests; 0 = one second.
	Pause time.Duration
	Now   func() time.Time
	Logf  func(string, ...any)
}

// Connector implements listing.Connector.
type Connector struct {
	cfg          Config
	mu           sync.Mutex
	lastComplete bool
}

// NewConnector fills defaults.
func NewConnector(cfg Config) *Connector {
	if cfg.APIBase == "" {
		cfg.APIBase = DefaultAPIBase
	}
	cfg.APIBase = strings.TrimRight(cfg.APIBase, "/")
	cfg.StoreURL = strings.TrimRight(cfg.StoreURL, "/")
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = DefaultMaxPages
	}
	if cfg.Fetch == nil {
		cfg.Fetch = &webfetch.Client{}
	}
	if cfg.Pause == 0 {
		cfg.Pause = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Connector{cfg: cfg}
}

// SourceID returns "commerce7:<Name>".
func (c *Connector) SourceID() string { return SourceID + ":" + c.cfg.Name }

// FetchComplete reports whether the last Fetch walked the whole catalogue.
func (c *Connector) FetchComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastComplete
}

type page struct {
	Products []product `json:"products"`
	Total    *int      `json:"total"`
}

type product struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	SubTitle  string    `json:"subTitle"`
	Teaser    string    `json:"teaser"`
	Content   string    `json:"content"`
	Slug      string    `json:"slug"`
	Type      string    `json:"type"`
	WebStatus string    `json:"webStatus"`
	Wine      *wine     `json:"wine"`
	Variants  []variant `json:"variants"`
}

type wine struct {
	Type        string `json:"type"`
	Varietal    string `json:"varietal"`
	Region      string `json:"region"`
	Appellation string `json:"appellation"`
	CountryCode string `json:"countryCode"`
	Vintage     *int   `json:"vintage"`
}

type variant struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Price           int64  `json:"price"`
	ComparePrice    *int64 `json:"comparePrice"`
	VolumeInML      *int   `json:"volumeInML"`
	HasInventory    bool   `json:"hasInventory"`
	InventoryPolicy string `json:"inventoryPolicy"`
}

// Fetch walks the catalogue and returns one Raw per purchasable wine variant.
func (c *Connector) Fetch(ctx context.Context) ([]listing.Raw, error) {
	c.mu.Lock()
	c.lastComplete = false
	c.mu.Unlock()
	now := c.cfg.Now()
	skipped := map[string]int{}
	var out []listing.Raw

	if c.cfg.FixturePath != "" {
		data, err := os.ReadFile(c.cfg.FixturePath)
		if err != nil {
			return nil, fmt.Errorf("commerce7 %s: %w", c.cfg.Name, err)
		}
		pg, err := decode(data)
		if err != nil {
			return nil, fmt.Errorf("commerce7 %s: %w", c.cfg.Name, err)
		}
		out = c.mapProducts(pg.Products, now, skipped)
		c.setComplete(true)
		c.logSkips(len(out), skipped)
		return out, nil
	}
	if c.cfg.Tenant == "" {
		return nil, errors.New("commerce7: no tenant configured")
	}

	seen, complete := 0, false
	for p := 1; p <= c.cfg.MaxPages; p++ {
		if p > 1 {
			if err := sleepCtx(ctx, c.cfg.Pause); err != nil {
				return nil, err
			}
		}
		u := fmt.Sprintf("%s/product/for-web?limit=%d&page=%d", c.cfg.APIBase, PageSize, p)
		data, err := c.cfg.Fetch.Get(ctx, u, map[string]string{"tenant": c.cfg.Tenant})
		if err != nil {
			return nil, fmt.Errorf("commerce7 %s: %w", c.cfg.Name, err)
		}
		pg, err := decode(data)
		if err != nil {
			return nil, fmt.Errorf("commerce7 %s page %d: %w", c.cfg.Name, p, err)
		}
		seen += len(pg.Products)
		out = append(out, c.mapProducts(pg.Products, now, skipped)...)
		if len(pg.Products) < PageSize || (pg.Total != nil && seen >= *pg.Total) {
			complete = true
			break
		}
	}
	if !complete && c.cfg.Logf != nil {
		c.cfg.Logf("commerce7 %s: TRUNCATED at %d pages (%d products) -- raise maxPages", c.cfg.Name, c.cfg.MaxPages, seen)
	}
	c.setComplete(complete)
	c.logSkips(len(out), skipped)
	return out, nil
}

func decode(data []byte) (page, error) {
	var pg page
	if err := json.Unmarshal(data, &pg); err != nil {
		return page{}, fmt.Errorf("response shape changed or not JSON: %w", err)
	}
	if pg.Products == nil {
		return page{}, errors.New("response has no products array (shape changed?)")
	}
	return pg, nil
}

func (c *Connector) mapProducts(ps []product, now time.Time, skipped map[string]int) []listing.Raw {
	var out []listing.Raw
	for _, p := range ps {
		switch {
		case !strings.EqualFold(p.Type, "Wine"):
			skipped["type "+p.Type]++
			continue
		case p.WebStatus != "" && !strings.EqualFold(p.WebStatus, "Available"):
			skipped["not available"]++
			continue
		case len(p.Variants) == 0:
			skipped["no variants (library listing)"]++
			continue
		}
		for _, v := range p.Variants {
			switch {
			case v.Price <= 0:
				skipped["no price"]++
				continue
			case !v.HasInventory && strings.EqualFold(v.InventoryPolicy, "Dont Sell"):
				skipped["sold out"]++
				continue
			}
			out = append(out, c.raw(p, v, len(p.Variants), now))
		}
	}
	return out
}

func (c *Connector) raw(p product, v variant, variants int, now time.Time) listing.Raw {
	aspects := map[string]string{}
	if w := p.Wine; w != nil {
		if w.Varietal != "" && !strings.EqualFold(w.Varietal, "blend") {
			aspects["varietal"] = w.Varietal
		}
		if w.Type != "" {
			aspects["wine_type"] = strings.ToLower(w.Type)
		}
		if w.Appellation != "" {
			aspects["appellation"] = w.Appellation
		}
		if w.Region != "" {
			aspects["region"] = w.Region
		}
		if w.Vintage != nil && *w.Vintage > 1800 {
			aspects["vintage"] = strconv.Itoa(*w.Vintage)
		}
	}
	if v.VolumeInML != nil && *v.VolumeInML > 0 {
		aspects["bottle_ml"] = strconv.Itoa(*v.VolumeInML)
	}
	if v.ComparePrice != nil && *v.ComparePrice > v.Price {
		aspects["compare_at_cents"] = strconv.FormatInt(*v.ComparePrice, 10)
	}
	title := p.Title
	// A product sold in several sizes gets one listing per size; name it.
	if variants > 1 && v.Title != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(v.Title)) {
		title += " " + v.Title
	}
	link := ""
	if c.cfg.StoreURL != "" && p.Slug != "" {
		link = c.cfg.StoreURL + "/product/" + url.PathEscape(p.Slug)
	}
	return listing.Raw{
		SourceID:   c.SourceID(),
		SourceKey:  v.ID,
		SourceURL:  link,
		Title:      title,
		Body:       strings.TrimSpace(stripTags(p.Teaser + " " + p.Content)),
		PriceCents: v.Price,
		Currency:   "USD",
		Aspects:    aspects,
		SeenAt:     now,
	}
}

func (c *Connector) setComplete(v bool) {
	c.mu.Lock()
	c.lastComplete = v
	c.mu.Unlock()
}

func (c *Connector) logSkips(kept int, skipped map[string]int) {
	if len(skipped) > 0 && c.cfg.Logf != nil {
		c.cfg.Logf("commerce7 %s: %d wine listings, skipped %v", c.cfg.Name, kept, skipped)
	}
}

// stripTags turns an HTML fragment into text for the (untrusted) body.
func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
			b.WriteRune(' ')
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
