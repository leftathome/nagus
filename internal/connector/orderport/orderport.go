// Package orderport reads a winery's OrderPort web store catalogue
// (nagus-29b).
//
// OrderPort hosts stores at <winery>.orderport.net (Hedges, Woodward Canyon,
// Long Shadows among WA producers fingerprinted 2026-09-21). There is no JSON
// feed and no schema.org Product data; the catalogue page itself is regular,
// server-rendered HTML, one card per product:
//
//	<a class="prod-title" name="<code>" href=".../product-details/<code>/<slug>">NAME</a>
//	... description ...
//	<span class="sale-price">$58.50</span><span class="price old-price">$65.00</span>
//	<span class="price-descr"> / 750ml</span>
//
// so this reads exactly those elements. A card with no add-to-cart block is not
// purchasable and is skipped. One page is read per fetch (the store lists its
// whole catalogue on /wines/All-Wines); a page with no cards at all is a loud
// error, because a redesigned store must not look like an empty one.
package orderport

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/connector/webfetch"
	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the connector family; a configured source is "orderport:<Name>".
const SourceID = "orderport"

// DefaultCatalogPath lists every product on an OrderPort store.
const DefaultCatalogPath = "/wines/All-Wines"

// Config configures one store.
type Config struct {
	Name string
	// StoreURL is the store root, e.g. "https://hedgesfamilyestate.orderport.net".
	StoreURL string
	// CatalogPath overrides DefaultCatalogPath.
	CatalogPath string
	FixturePath string
	Fetch       *webfetch.Client
	Now         func() time.Time
	Logf        func(string, ...any)
}

// Connector implements listing.Connector.
type Connector struct {
	cfg          Config
	mu           sync.Mutex
	lastComplete bool
}

// NewConnector fills defaults.
func NewConnector(cfg Config) *Connector {
	cfg.StoreURL = strings.TrimRight(cfg.StoreURL, "/")
	if cfg.CatalogPath == "" {
		cfg.CatalogPath = DefaultCatalogPath
	}
	if cfg.Fetch == nil {
		cfg.Fetch = &webfetch.Client{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Connector{cfg: cfg}
}

// SourceID returns "orderport:<Name>".
func (c *Connector) SourceID() string { return SourceID + ":" + c.cfg.Name }

// FetchComplete reports whether the last Fetch parsed the catalogue page.
func (c *Connector) FetchComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastComplete
}

var (
	reCardStart = regexp.MustCompile(`<h2 class="prod-name">`)
	reTitle     = regexp.MustCompile(`(?s)<a[^>]*class="prod-title"[^>]*>(.*?)</a>`)
	reHref      = regexp.MustCompile(`href="([^"]+)"`)
	reName      = regexp.MustCompile(`name="([^"]+)"`)
	reSale      = regexp.MustCompile(`class="sale-price"[^>]*>\s*([^<]+)<`)
	reOld       = regexp.MustCompile(`class="price old-price"[^>]*>\s*([^<]+)<`)
	rePlain     = regexp.MustCompile(`class="price"[^>]*>\s*([^<]+)<`)
	reML        = regexp.MustCompile(`(?i)class="price-descr".*?(\d{3,4})\s*ml`)
	reContent   = regexp.MustCompile(`(?s)<div class="prod-content">(.*?)<fieldset`)
	reTags      = regexp.MustCompile(`(?s)<[^>]*>`)
)

// Fetch reads the catalogue page and returns one Raw per purchasable product.
func (c *Connector) Fetch(ctx context.Context) ([]listing.Raw, error) {
	c.setComplete(false)
	var data []byte
	var err error
	switch {
	case c.cfg.FixturePath != "":
		data, err = os.ReadFile(c.cfg.FixturePath)
	case c.cfg.StoreURL == "":
		return nil, errors.New("orderport: no storeUrl configured")
	default:
		data, err = c.cfg.Fetch.Get(ctx, c.cfg.StoreURL+c.cfg.CatalogPath, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("orderport %s: %w", c.cfg.Name, err)
	}
	page := string(data)
	starts := reCardStart.FindAllStringIndex(page, -1)
	if len(starts) == 0 && c.cfg.FixturePath == "" {
		// Stores differ in which listing carries the catalogue (Hedges shows
		// nothing on All-Wines and everything on current-releases). Follow the
		// store's own /wines/<listing> links, at most three, first with cards.
		page, starts = c.discover(ctx, page)
	}
	if len(starts) == 0 {
		return nil, fmt.Errorf("orderport %s: no product cards on the catalogue page (store redesigned?)", c.cfg.Name)
	}
	now := c.cfg.Now()
	skipped := map[string]int{}
	var out []listing.Raw
	for i, st := range starts {
		end := len(page)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		r, why := c.card(page[st[0]:end], now)
		if why != "" {
			skipped[why]++
			continue
		}
		out = append(out, r)
	}
	if len(skipped) > 0 && c.cfg.Logf != nil {
		c.cfg.Logf("orderport %s: %d products, skipped %v", c.cfg.Name, len(out), skipped)
	}
	c.setComplete(true)
	return out, nil
}

var reListing = regexp.MustCompile(`href="(?:https?://[^"/]+)?(/wines/[A-Za-z0-9_-]+)"`)

// discover tries the store's own listing links when the configured page has no
// product cards, returning the first page that has some.
func (c *Connector) discover(ctx context.Context, page string) (string, [][]int) {
	tried := 0
	seen := map[string]bool{c.cfg.CatalogPath: true}
	for _, m := range reListing.FindAllStringSubmatch(page, -1) {
		path := m[1]
		if seen[path] || tried >= 3 {
			continue
		}
		seen[path] = true
		tried++
		data, err := c.cfg.Fetch.Get(ctx, c.cfg.StoreURL+path, nil)
		if err != nil {
			continue
		}
		if starts := reCardStart.FindAllStringIndex(string(data), -1); len(starts) > 0 {
			if c.cfg.Logf != nil {
				c.cfg.Logf("orderport %s: no products on %s; using %s (set catalogPath to skip this lookup)", c.cfg.Name, c.cfg.CatalogPath, path)
			}
			return string(data), starts
		}
	}
	return page, nil
}

// card parses one product card; a non-empty reason means it was skipped.
func (c *Connector) card(card string, now time.Time) (listing.Raw, string) {
	tm := reTitle.FindStringSubmatch(card)
	if tm == nil {
		return listing.Raw{}, "no title"
	}
	anchor := tm[0]
	title := strings.TrimSpace(html.UnescapeString(reTags.ReplaceAllString(tm[1], "")))
	if !strings.Contains(card, `class="add-to-cart"`) {
		return listing.Raw{}, "not purchasable"
	}
	var price, list int64
	if m := reSale.FindStringSubmatch(card); m != nil {
		price = dollarsToCents(m[1])
		if o := reOld.FindStringSubmatch(card); o != nil {
			list = dollarsToCents(o[1])
		}
	} else if m := rePlain.FindStringSubmatch(card); m != nil {
		price = dollarsToCents(m[1])
	}
	if price <= 0 {
		return listing.Raw{}, "no price"
	}
	aspects := map[string]string{}
	if list > price {
		aspects["compare_at_cents"] = strconv.FormatInt(list, 10)
	}
	if m := reML.FindStringSubmatch(card); m != nil {
		aspects["bottle_ml"] = m[1]
	}
	key := ""
	if m := reName.FindStringSubmatch(anchor); m != nil {
		key = m[1]
	}
	link := ""
	if m := reHref.FindStringSubmatch(anchor); m != nil {
		link = html.UnescapeString(m[1])
		if key == "" {
			key = link
		}
	}
	body := ""
	if m := reContent.FindStringSubmatch(card); m != nil {
		body = strings.Join(strings.Fields(html.UnescapeString(reTags.ReplaceAllString(m[1], " "))), " ")
	}
	return listing.Raw{
		SourceID:   c.SourceID(),
		SourceKey:  key,
		SourceURL:  link,
		Title:      title,
		Body:       body,
		PriceCents: price,
		Currency:   "USD",
		Aspects:    aspects,
		SeenAt:     now,
	}, ""
}

func dollarsToCents(s string) int64 {
	s = strings.NewReplacer("$", "", ",", "", " ", "").Replace(strings.TrimSpace(html.UnescapeString(s)))
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(f*100 + 0.5)
}

func (c *Connector) setComplete(v bool) {
	c.mu.Lock()
	c.lastComplete = v
	c.mu.Unlock()
}
