// Package shopify implements a nagus-direct listing.Connector over the public,
// unauthenticated `/products.json` feed every Shopify storefront exposes. It is
// ONE generic connector configured per retailer, not a family of per-store
// connectors: the store variation is base URL, name and an allow-filter, all
// configuration rather than code.
//
// It exists because eBay Browse is the only eBay path with keyword search and our
// production keyset is not granted that API (nagus-9nx), leaving the hdd category
// with no working live source. See docs/design/2026-07-26-shopify-connector.md.
//
// # One Raw per VARIANT
//
// Each variant is a distinct sellable item (its own price, sku and availability),
// so Fetch emits one listing.Raw per variant with SourceKey "<productID>:<variantID>".
//
// # What real catalogs look like (serverpartdeals, captured 2026-07-30)
//
// Three things the published Shopify schema does not tell you, learned from live
// data and encoded here:
//
//   - **Capacity is NOT in the product title.** Titles are model-number strings
//     like "Western Digital Ultrastar DC HC580 WUH722424AL5201 0F62801". The
//     capacity lives in `product_type` ("Hard Drives > 24TB > 3.5 > SAS > 7.2K
//     RPM") and in a structured `capacity:24TB` tag. Since the hdd extractor reads
//     capacity from the title, a connector that did not surface capacity as a
//     typed aspect would have every one of its items dropped by the capacity
//     hard-filter. So capacity is parsed here and passed through Aspects.
//   - **Condition is in the tags**, not a field: a `000CardTitle:Refurbished ...`
//     / `New ...` convention. The same drive appears as SEPARATE PRODUCTS for
//     new/refurbished, so condition cannot be inferred per-variant.
//   - **product_type prefixes are inconsistent** for the same category: both
//     "Hard Drives > 18TB > ..." and "HDDs > 18TB > ..." occur. An allow-filter
//     must therefore accept a LIST of prefixes.
//
// # Large mixed catalogues: walk a collection
//
// A fetch that stops at MaxPages never refreshes the products behind the cap
// and never lets offer expiry run. serverpartdeals is the case in point: more
// than 10,000 products, hard drives scattered across every page. Rather than
// walking 40+ pages an hour, such a store is pointed at the storefront
// collections holding the category (Config.Collections), which Shopify
// serves through the same products.json shape (nagus-bu2). One collection is
// often not enough: serverpartdeals needs two to cover every drive.
//
// # Rate limiting
//
// serverpartdeals returns HTTP 429 with a plain-text "local_rate_limited" body and
// is aggressive about it (five consecutive 429s at 45s intervals during the
// capture). Poll politely -- hourly at most, per store. Cadence between fetches
// is the caller's responsibility; WITHIN a fetch the connector paces itself,
// waiting PageDelay between successive pages (nagus-bu2), so walking a large
// catalogue to its end is a slow trickle rather than a burst.
//
// # Trust boundary
//
// Title, Body and Aspects VALUES are UNTRUSTED free text per internal/listing's
// contract; this connector maps fields and strips HTML from body_html, but does
// not sanitize -- that is the glovebox boundary's job. PriceCents, Currency and
// the derived capacity are trusted scalars once computed here.
package shopify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the stable connector-family identity. A configured Connector always
// reports "shopify:<Name>" so each store is its own source with isolated
// freshness/purge and isolated failure.
const SourceID = "shopify"

const (
	// ProductsPath is the public storefront feed. No auth, no key.
	ProductsPath = "/products.json"
	// DefaultLimit is Shopify's maximum page size.
	DefaultLimit = 250
	// DefaultMaxPages bounds a single Fetch. Specialist retailers run small
	// catalogs; this stops a mis-pointed source walking an enormous store.
	DefaultMaxPages = 4
	// DefaultMaxRetries bounds per-page retries after a 429.
	DefaultMaxRetries = 3
	// MaxRetryWait caps how long we will honour a Retry-After, so a hostile or
	// mistaken header cannot wedge a source's ingest goroutine for hours.
	MaxRetryWait = 2 * time.Minute
	// DefaultCurrency -- products.json omits currency entirely.
	DefaultCurrency = "USD"
	// DefaultPageDelay is the courtesy pause between successive pages of one
	// fetch. A burst of back-to-back pages is what trips serverpartdeals'
	// limiter; at 3s a 30-page walk spends 29 x 3s = 87s pausing (plus the
	// requests themselves), far below any per-second budget. A single-page
	// store never waits.
	DefaultPageDelay = 3 * time.Second
	// DefaultUserAgent identifies the poller politely.
	DefaultUserAgent = "nagus/0.2 (+https://github.com/leftathome/nagus)"
)

// collectionHandleRe is a Shopify collection handle. Checked because the
// handle is spliced into a URL path.
var collectionHandleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidCollectionHandle reports whether s is a well-formed collection handle,
// so configuration can be rejected at startup rather than on first fetch.
func ValidCollectionHandle(s string) bool {
	return collectionHandleRe.MatchString(strings.TrimSpace(s))
}

// ErrRateLimited is a 429 from the storefront. Surfaced distinctly so an operator
// can tell throttling from a broken store.
var ErrRateLimited = errors.New("shopify: rate limited by storefront")

// Config configures one store.
type Config struct {
	// Name is the operator-chosen store name; SourceID() is "shopify:<Name>".
	// Required -- per-store identity is what gives isolated freshness.
	Name string
	// BaseURL is the storefront root, e.g. "https://serverpartdeals.com".
	BaseURL string

	// ProductTypePrefixes, when non-empty, keeps only products whose
	// product_type starts with one of these (case-insensitive). This is the
	// allow-filter that stops a mixed catalog (HDDs AND SSDs) pulling the whole
	// store downstream. Note real catalogs use INCONSISTENT prefixes for one
	// category, so pass every spelling: {"Hard Drives", "HDDs"}.
	ProductTypePrefixes []string
	// Collection, when set, walks one storefront collection's feed
	// (/collections/<handle>/products.json) instead of the whole catalogue's.
	// It is how a store whose catalogue is far larger than the category we
	// want is walked COMPLETELY and cheaply (nagus-bu2): serverpartdeals has
	// more than 10,000 products, 40+ pages, but its "all-hard-drives"
	// collection is every in-stock hard drive in 2 pages (measured
	// 2026-09-25) -- except the "HDDs > ..." drives; see Collections, of
	// which this is a single-entry alias. The allow-filter still applies on
	// top. A handle is a Shopify slug: lowercase letters, digits and hyphens
	// only.
	Collection string
	// Collections walks several collections in one fetch, for a category a
	// single collection does not cover: serverpartdeals files 7 in-stock
	// drives typed "HDDs > ..." only under "hard-drives", and 1 only under
	// "all-hard-drives" (measured 2026-09-25). Products are deduped by id
	// across collections, and the fetch is complete only when EVERY
	// collection's walk is. Collection, if also set, is walked first.
	Collections []string
	// IncludeUnavailable emits variants with available=false. Default false:
	// an out-of-stock drive is not an actionable deal, and surfacing it as one
	// is noise.
	IncludeUnavailable bool

	// --- product identity (cross-seller dedup) ---
	//
	// These are DECLARED PER STORE rather than guessed, because guessing mints
	// false product identities. Concretely: serverpartdeals' `vendor` really is
	// the manufacturer ("Western Digital"), but waterpanther's is "Water
	// Panther" -- the STORE's own house label on generic OEM-tray drives whose
	// actual manufacturer is not stated anywhere. Reading vendor as a brand
	// there would group unrelated drives under a reseller name and then present
	// them as one product. A store with no real identity data should emit NO
	// hints and stay ungrouped, which is an honest answer.
	//
	// BrandTag is the tag prefix carrying the manufacturer, e.g. "brand:".
	// Empty means this store does not state a manufacturer.
	BrandTag string
	// SKUIsMPN declares that variant.sku IS (or embeds) the manufacturer part
	// number. Off by default: most stores' SKUs are internal codes.
	SKUIsMPN bool
	// SKUSuffixes are trailing SKU segments to strip before using it as an MPN.
	// serverpartdeals encodes CONDITION in the suffix (_SR seller-refurbished,
	// _MR manufacturer-recertified, _NB new-bulk) so the same physical product
	// has three SKUs; stripping them is what lets the three group as one
	// product with three offers, which is exactly the intent.
	SKUSuffixes []string

	// MaxRetries bounds how many times a single page is retried after a 429.
	// Storefronts send `Retry-After` (serverpartdeals sends 60), so the wait is
	// the server's own instruction rather than a guess -- retrying is the polite
	// behaviour a rate limiter expects, not hitting it harder. Defaults to
	// DefaultMaxRetries; 0 disables retrying.
	MaxRetries int
	// Sleep is the delay hook, injectable so tests do not actually wait.
	Sleep func(ctx context.Context, d time.Duration) error

	// MaxPages bounds pagination. Defaults to DefaultMaxPages. It is a guard
	// against a mis-pointed source, not a sampling knob: set it ABOVE the
	// store's real page count, because a fetch that stops at the cap never
	// refreshes the tail and never lets offer expiry run (nagus-bu2).
	MaxPages int
	// PageDelay is the courtesy pause before every page after the first.
	// Defaults to DefaultPageDelay; negative disables it (tests).
	PageDelay time.Duration
	// Limit is the page size. Defaults to DefaultLimit.
	Limit int

	HTTPClient *http.Client
	UserAgent  string
	Now        func() time.Time
	// Logf reports conditions the caller needs to know but which are not errors
	// -- notably a paginated fetch stopping at MaxPages with more inventory
	// behind it. nil disables logging.
	Logf func(format string, args ...any)

	// FixturePath, when non-empty, reads a local products.json instead of any
	// network call -- the offline proving path.
	FixturePath string
}

// Connector implements listing.Connector over one store's products.json.
type Connector struct {
	cfg Config

	mu           sync.Mutex
	lastComplete bool
}

// FetchComplete reports whether the most recent Fetch walked the store's
// catalogue to its end, rather than stopping at the page cap with inventory
// still behind it.
//
// It exists because CALLERS MUST NOT DRAW ABSENCE CONCLUSIONS FROM A PARTIAL
// FETCH. "This offer did not appear, so it is gone" is only sound if we actually
// looked at everything; after a truncated or rate-limited walk it is false, and
// acting on it would expire live, purchasable listings.
func (c *Connector) FetchComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastComplete
}

// NewConnector builds a Connector, filling in defaults.
func NewConnector(cfg Config) *Connector {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultUserAgent
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = DefaultMaxPages
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.Limit <= 0 {
		cfg.Limit = DefaultLimit
	}
	if cfg.PageDelay == 0 {
		cfg.PageDelay = DefaultPageDelay
	} else if cfg.PageDelay < 0 {
		cfg.PageDelay = 0
	}
	cfg.Collection = strings.TrimSpace(cfg.Collection)
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	return &Connector{cfg: cfg}
}

func (c *Connector) logf(format string, args ...any) {
	if c.cfg.Logf != nil {
		c.cfg.Logf(format, args...)
	}
}

// SourceID returns "shopify:<Name>", or the bare family constant if unnamed.
func (c *Connector) SourceID() string {
	if c.cfg.Name != "" {
		return SourceID + ":" + c.cfg.Name
	}
	return SourceID
}

// Fetch walks the store's products.json and returns one Raw per surviving
// variant. A page returning zero products ends pagination early.
func (c *Connector) Fetch(ctx context.Context) ([]listing.Raw, error) {
	now := c.cfg.Now()
	var raws []listing.Raw
	// A fetch that fails part-way must not leave the PREVIOUS run's
	// "complete" standing (commerce7 and vinoshipper reset it the same way).
	c.mu.Lock()
	c.lastComplete = false
	c.mu.Unlock()

	if c.cfg.FixturePath != "" {
		data, err := os.ReadFile(c.cfg.FixturePath)
		if err != nil {
			return nil, fmt.Errorf("shopify: read fixture %s: %w", c.cfg.FixturePath, err)
		}
		prods, err := decodeProducts(data)
		if err != nil {
			return nil, err
		}
		return c.mapProducts(prods, now), nil
	}

	if c.cfg.BaseURL == "" {
		return nil, errors.New("shopify: no base url configured")
	}
	walks := c.walks()
	for _, h := range walks {
		if h != "" && !ValidCollectionHandle(h) {
			return nil, fmt.Errorf("shopify: collection %q is not a Shopify handle (lowercase letters, digits, hyphens)", h)
		}
	}
	// The run is complete only if EVERY walk reached its end. A product in
	// more than one collection is emitted once (seen, by product id).
	complete := true
	seen := map[int64]bool{}
	requested := false
	for _, h := range walks {
		r, ok, err := c.walk(ctx, h, now, seen, &requested)
		if err != nil {
			return nil, err
		}
		raws = append(raws, r...)
		complete = complete && ok
	}
	c.mu.Lock()
	c.lastComplete = complete
	c.mu.Unlock()
	return raws, nil
}

// walks lists the feeds one Fetch reads: each configured collection once, in
// order, or "" for the whole catalogue.
func (c *Connector) walks() []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range append([]string{c.cfg.Collection}, c.cfg.Collections...) {
		if h = strings.TrimSpace(h); h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// walk pages through one feed (a collection, or the catalogue for "") and
// reports whether it reached the feed's end. requested tracks whether any
// page has been fetched in this Fetch, so the courtesy pause also separates
// the last page of one collection from the first of the next.
//
// complete records whether we walked the feed to its end. Exhausting MaxPages
// while the last page was still FULL means there is more inventory we did not
// fetch -- and a bounded fetch that says nothing is the worst kind of bug,
// because partial coverage is indistinguishable from full coverage in the
// output. So the cap is reported, never silent.
func (c *Connector) walk(ctx context.Context, handle string, now time.Time, seen map[int64]bool, requested *bool) ([]listing.Raw, bool, error) {
	var raws []listing.Raw
	complete := false
	products := 0
	for page := 1; page <= c.cfg.MaxPages; page++ {
		if *requested && c.cfg.PageDelay > 0 {
			// Courtesy pacing: never fetch pages back to back.
			if err := c.cfg.Sleep(ctx, c.cfg.PageDelay); err != nil {
				return nil, false, err
			}
		}
		*requested = true
		data, err := c.fetchPageWithRetry(ctx, handle, page)
		if err != nil {
			return nil, false, err
		}
		prods, err := decodeProducts(data)
		if err != nil {
			return nil, false, err
		}
		if len(prods) == 0 {
			if page == 1 && handle != "" {
				// An EMPTY collection is not an empty store: the collection
				// was emptied, hidden or renamed (Shopify answers an unknown
				// handle with an empty list, not a 404). Calling that
				// complete would expire every offer the source holds.
				c.logf("shopify %s: collection %q returned NO products on page 1 -- treating the fetch as incomplete (offer expiry skipped); check that the collection still exists and is published",
					c.SourceID(), handle)
				return raws, false, nil
			}
			complete = true
			break
		}
		products += len(prods)
		fresh := prods[:0:0]
		for _, p := range prods {
			if p.ID != 0 && seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			fresh = append(fresh, p)
		}
		raws = append(raws, c.mapProducts(fresh, now)...)
		if len(prods) < c.cfg.Limit {
			// Short page: this was the last one.
			complete = true
			break
		}
	}
	if !complete {
		// products is what the store returned; raws is what survived the
		// allow-filter and availability as per-variant listings. Reporting
		// the second as "products" understated the walk (nagus-bu2).
		feed := "catalogue"
		if handle != "" {
			feed = "collection " + strconv.Quote(handle)
		}
		c.logf("shopify %s: %s TRUNCATED at the %d-page cap (%d store products read, %d listings kept, last page full) -- products past the cap are never refreshed and offer expiry is skipped; raise maxPages above the store's page count",
			c.SourceID(), feed, c.cfg.MaxPages, products, len(raws))
	}
	return raws, complete, nil
}

// fetchPageWithRetry retries a rate-limited page, waiting as long as the SERVER
// asked via Retry-After rather than guessing. Storefronts here send it (60s),
// so this is obeying a rate limiter, not defeating one.
//
// The wait is capped by MaxRetryWait so a mistaken or hostile header cannot
// wedge a source's ingest goroutine, and it honours context cancellation so a
// shutdown is not blocked by a sleeping fetch. Errors other than rate limiting
// are returned immediately -- retrying a 404 or a decode failure just delays the
// same answer.
func (c *Connector) fetchPageWithRetry(ctx context.Context, handle string, page int) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		data, wait, err := c.fetchPageOnce(ctx, handle, page)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, ErrRateLimited) {
			return nil, err
		}
		lastErr = err
		if attempt == c.cfg.MaxRetries {
			break
		}
		if wait <= 0 {
			wait = 30 * time.Second
		}
		if wait > MaxRetryWait {
			wait = MaxRetryWait
		}
		c.logf("shopify %s: page %d rate limited, waiting %s as instructed (attempt %d/%d)",
			c.SourceID(), page, wait, attempt+1, c.cfg.MaxRetries)
		if serr := c.cfg.Sleep(ctx, wait); serr != nil {
			return nil, serr
		}
	}
	return nil, lastErr
}

// sleepCtx waits for d, or returns early if the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fetchPageOnce returns the body, or on a 429 the server's requested wait.
func (c *Connector) fetchPageOnce(ctx context.Context, handle string, page int) ([]byte, time.Duration, error) {
	path := ProductsPath
	if handle != "" {
		path = "/collections/" + handle + ProductsPath
	}
	url := fmt.Sprintf("%s%s?limit=%d&page=%d", c.cfg.BaseURL, path, c.cfg.Limit, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("shopify: build request: %w", err)
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("shopify: request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("shopify: read response body: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return body, 0, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, retryAfter(resp), fmt.Errorf("%w (page %d): %s", ErrRateLimited, page, truncate(body, 80))
	default:
		return nil, 0, fmt.Errorf("shopify: products.json returned status %d: %s", resp.StatusCode, truncate(body, 160))
	}
}

// decodeProducts parses a products.json page. Real storefronts can emit bytes
// that are not valid UTF-8 (a CP1252 smart quote inside body_html, seen live), so
// the payload is sanitized to valid UTF-8 before decoding rather than failing.
func decodeProducts(data []byte) ([]product, error) {
	var page productsPage
	if err := json.Unmarshal(toValidUTF8(data), &page); err != nil {
		return nil, fmt.Errorf("shopify: decode products.json: %w", err)
	}
	return page.Products, nil
}

// mapProducts applies the allow-filter and flattens products to per-variant Raws.
func (c *Connector) mapProducts(prods []product, now time.Time) []listing.Raw {
	out := make([]listing.Raw, 0, len(prods))
	for _, p := range prods {
		if !c.allowed(p) {
			continue
		}
		cond := conditionFromTags(p.Tags)
		for _, v := range p.Variants {
			var capTB string
			var hasCap bool
			if !v.Available && !c.cfg.IncludeUnavailable {
				continue
			}
			if p.ID == 0 || v.ID == 0 {
				// No stable source-native key -> no provenance -> not a valid Raw.
				continue
			}
			// Capacity is a PRODUCT-level field, but a multi-variant product can
			// sell several capacities under one listing (the variant title/option
			// carries it). Prefer the variant's own capacity so each Raw reports
			// what that variant actually is; fall back to the product's.
			vTB, vOK := variantCapacityTB(v)
			if vOK {
				capTB, hasCap = vTB, true
			} else if pTB, pOK := capacityTB(p); pOK {
				capTB, hasCap = pTB, true
			}
			aspects := map[string]string{}
			if hasCap {
				// Typed capacity: the hdd extractor cannot recover this from the
				// title, so it MUST travel as an aspect. See package comment.
				aspects["capacity_tb"] = capTB
			}
			if p.Vendor != "" {
				aspects["vendor"] = p.Vendor
			}
			if p.ProductType != "" {
				aspects["product_type"] = p.ProductType
			}
			if v.SKU != "" {
				aspects["sku"] = v.SKU
			}
			if vt := strings.TrimSpace(v.Title); vt != "" && !strings.EqualFold(vt, "Default Title") {
				aspects["variant"] = vt
			}
			if b := c.brandOf(p); b != "" {
				aspects["brand"] = b
			}
			if m := c.mpnOf(p.Title, v); m != "" {
				aspects["mpn"] = m
			}
			if t, err := time.Parse(time.RFC3339, p.PublishedAt); err == nil {
				aspects["published_at"] = t.UTC().Format("2006-01-02")
			}
			if cmp := compareAtCents(v.CompareAtPrice); cmp > priceCents(v.Price) && priceCents(v.Price) > 0 {
				aspects["compare_at_cents"] = strconv.FormatInt(cmp, 10)
			}
			out = append(out, listing.Raw{
				SourceID:     c.SourceID(),
				SourceKey:    fmt.Sprintf("%d:%d", p.ID, v.ID),
				SourceURL:    c.productURL(p.Handle),
				Title:        p.Title,               // UNTRUSTED
				Body:         stripHTML(p.BodyHTML), // UNTRUSTED, tags stripped to text
				PriceCents:   priceCents(v.Price),
				Currency:     DefaultCurrency,
				ConditionRaw: cond,
				Aspects:      aspects,
				SeenAt:       now,
			})
		}
	}
	return out
}

// allowed reports whether a product passes the configured product_type
// allow-filter. An empty filter allows everything.
func (c *Connector) allowed(p product) bool {
	if len(c.cfg.ProductTypePrefixes) == 0 {
		return true
	}
	pt := strings.ToLower(strings.TrimSpace(p.ProductType))
	for _, pre := range c.cfg.ProductTypePrefixes {
		if strings.HasPrefix(pt, strings.ToLower(strings.TrimSpace(pre))) {
			return true
		}
	}
	return false
}

func (c *Connector) productURL(handle string) string {
	if handle == "" || c.cfg.BaseURL == "" {
		return c.cfg.BaseURL
	}
	return c.cfg.BaseURL + "/products/" + handle
}

// --- wire types ---------------------------------------------------------------

type productsPage struct {
	Products []product `json:"products"`
}

type product struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Handle      string    `json:"handle"`
	BodyHTML    string    `json:"body_html"`
	Vendor      string    `json:"vendor"`
	ProductType string    `json:"product_type"`
	Tags        []string  `json:"tags"`
	Variants    []variant `json:"variants"`
	// PublishedAt is when the store published the product (RFC 3339). A
	// recent date is a new-release signal (nagus-cux).
	PublishedAt string `json:"published_at"`
}

type variant struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Price string `json:"price"` // string in the wire format, e.g. "799.00"
	// CompareAtPrice is the store's own "was" price, set when the variant is
	// on sale; null otherwise. Raw, because stores send a string, null, or
	// occasionally a number, and one odd product must not fail the page.
	CompareAtPrice json.RawMessage `json:"compare_at_price"`
	SKU            string          `json:"sku"`
	Available      bool            `json:"available"`
	Option1        string          `json:"option1"`
}

// brandOf returns the manufacturer from the configured brand tag, or "" when the
// store does not state one. It deliberately does NOT fall back to `vendor`: see
// Config.BrandTag for why that would be actively wrong at some stores.
func (c *Connector) brandOf(p product) string {
	if c.cfg.BrandTag == "" {
		return ""
	}
	for _, t := range p.Tags {
		if v, ok := cutPrefixFold(strings.TrimSpace(t), c.cfg.BrandTag); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return ""
}

// mpnOf returns the manufacturer part number a store's SKU embeds, or "" when
// this store's SKUs are not MPNs or none can be found.
//
// A SKU is not an MPN even when it starts with one (quark QUARK-02
// measurement, 2026-09-24): serverpartdeals SKUs carry seller segments after
// the part number -- `HUS726040AL4215-DELL_DELLG13`, `00JHTD_DELLG14_SR_12`,
// `KHK6XLSE960G-DELL_DELLG14_CONVERT_SR` -- so sending the whole SKU fragmented
// one drive into many quark "products" (266 keys over 61 base part numbers on
// the live store) and gave text matching nothing a title could ever contain.
// The part number is, in order:
//
//  1. the longest part-number-shaped token of the product TITLE that the SKU
//     starts with (followed by end, '-' or '_') -- this keeps a hyphenated
//     manufacturer number whole (`WD60EFRX-68MYMN1`) whenever the title
//     states it;
//  2. else the SKU's leading segment before the first '-' or '_', if it is
//     part-number-shaped (>= 6 chars with a letter and a digit): Dell-branded
//     titles name Dell's own number (`06WR5M`) while the SKU leads with the
//     maker's (`KHK6XLSE960G`);
//  3. else nothing -- no key is better than a wrong one.
//
// Configured condition suffixes (_SR/_MR/_NB) are stripped first.
func (c *Connector) mpnOf(title string, v variant) string {
	if !c.cfg.SKUIsMPN {
		return ""
	}
	sku := strings.TrimSpace(v.SKU)
	if sku == "" {
		return ""
	}
	for _, suf := range c.cfg.SKUSuffixes {
		if suf == "" {
			continue
		}
		if strings.HasSuffix(strings.ToUpper(sku), strings.ToUpper(suf)) {
			sku = sku[:len(sku)-len(suf)]
			break
		}
	}
	sku = strings.ToUpper(strings.TrimSpace(sku))
	best := ""
	for _, t := range partNumberTokens(title) {
		if len(t) <= len(best) || !strings.HasPrefix(sku, t) {
			continue
		}
		if len(sku) == len(t) || sku[len(t)] == '-' || sku[len(t)] == '_' {
			best = t
		}
	}
	if best != "" {
		return best
	}
	lead := sku
	if i := strings.IndexAny(sku, "-_"); i >= 0 {
		lead = sku[:i]
	}
	if len(lead) >= 6 && partNumberShaped(lead) {
		return lead
	}
	return ""
}

// partNumberTokens splits a title on whitespace and punctuation, keeping '-'
// inside a token, and returns the upper-cased tokens that look like part
// numbers.
func partNumberTokens(title string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToUpper(title), func(r rune) bool {
		return !(r == '-' || r == '.' || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) {
		f = strings.Trim(f, ".-")
		if len(f) >= 5 && partNumberShaped(f) {
			out = append(out, f)
		}
	}
	return out
}

// partNumberShaped reports a string holding both a letter and a digit.
func partNumberShaped(s string) bool {
	var letter, digit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			letter = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return letter && digit
}

// cutPrefixFold is strings.CutPrefix with case-insensitive matching.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// --- field derivation ---------------------------------------------------------

var (
	// capacityTagRe matches the structured "capacity:24TB" tag convention.
	capacityTagRe = regexp.MustCompile(`(?i)^capacity:\s*(\d+(?:\.\d+)?)\s*(TB|GB)$`)
	// capacityTypeRe pulls the capacity out of a breadcrumb product_type such as
	// "Hard Drives > 24TB > 3.5 > SAS > 7.2K RPM".
	capacityTypeRe = regexp.MustCompile(`(?i)>\s*(\d+(?:\.\d+)?)\s*(TB|GB)\s*>`)
)

// capacityTB derives the drive capacity in TB. It prefers the structured
// capacity: tag and falls back to the product_type breadcrumb. ok=false when
// neither carries one -- an absent fact, not an error.
func capacityTB(p product) (string, bool) {
	for _, t := range p.Tags {
		if m := capacityTagRe.FindStringSubmatch(strings.TrimSpace(t)); m != nil {
			if v, ok := normalizeTB(m[1], m[2]); ok {
				return v, true
			}
		}
	}
	if m := capacityTypeRe.FindStringSubmatch(p.ProductType); m != nil {
		if v, ok := normalizeTB(m[1], m[2]); ok {
			return v, true
		}
	}
	return "", false
}

// variantCapacityTB recovers a capacity from a variant's own title/option, which
// is where a multi-capacity product distinguishes its variants. ok=false for the
// common single-variant case (title "Default Title").
func variantCapacityTB(v variant) (string, bool) {
	for _, cand := range []string{v.Title, v.Option1} {
		cand = strings.TrimSpace(cand)
		if cand == "" || strings.EqualFold(cand, "Default Title") {
			continue
		}
		if m := bareCapacityRe.FindStringSubmatch(cand); m != nil {
			if tb, ok := normalizeTB(m[1], m[2]); ok {
				return tb, true
			}
		}
	}
	return "", false
}

// bareCapacityRe matches a standalone capacity like "16TB" or "960 GB".
var bareCapacityRe = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*(TB|GB)\b`)

// normalizeTB converts a (value, unit) capacity to TB, trimming trailing zeros.
func normalizeTB(num, unit string) (string, bool) {
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v <= 0 {
		return "", false
	}
	if strings.EqualFold(unit, "GB") {
		v /= 1000
	}
	return strconv.FormatFloat(v, 'f', -1, 64), true
}

// conditionFromTags recovers the source-native condition token. Real catalogs
// encode it in a "000CardTitle:Refurbished ..." tag convention rather than a
// field, and list new/refurbished as SEPARATE products.
func conditionFromTags(tags []string) string {
	for _, t := range tags {
		lt := strings.ToLower(t)
		switch {
		case strings.Contains(lt, "refurbished"), strings.Contains(lt, "recertified"):
			return "Refurbished"
		case strings.Contains(lt, "used"):
			return "Used"
		}
	}
	for _, t := range tags {
		if strings.Contains(strings.ToLower(t), "new") {
			return "New"
		}
	}
	return ""
}

// priceCents converts the wire price string ("799.00") to integer cents. An
// unparseable or nonpositive price yields 0 == unknown.
func priceCents(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return int64(v*100 + 0.5)
}

// --- helpers ------------------------------------------------------------------

// retryAfter reads the server's requested wait. Only the delta-seconds form is
// handled: it is what these storefronts send, and mis-parsing an HTTP-date into
// a huge wait would be worse than falling back to a sane default.
func retryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// stripHTML reduces body_html to plain text. The result is still UNTRUSTED free
// text; stripping markup is a shape change, not sanitization.
func stripHTML(s string) string {
	if s == "" {
		return ""
	}
	txt := tagRe.ReplaceAllString(s, " ")
	txt = html.UnescapeString(txt)
	return strings.TrimSpace(strings.Join(strings.Fields(txt), " "))
}

// toValidUTF8 replaces invalid UTF-8 bytes so encoding/json can decode a payload
// containing stray CP1252 bytes (observed live: 0x94 for an inch mark).
func toValidUTF8(b []byte) []byte {
	return []byte(strings.ToValidUTF8(string(b), "?"))
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// compareAtCents reads a variant's compare_at_price -- a JSON string, number or
// null -- as cents; 0 when absent or unparseable.
func compareAtCents(raw json.RawMessage) int64 {
	v := strings.TrimSpace(string(raw))
	if v == "" || v == "null" {
		return 0
	}
	return priceCents(strings.Trim(v, `"`))
}
