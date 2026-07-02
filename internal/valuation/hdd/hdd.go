// Package hdd is the $/TB valuation adapter for the "hdd" category. Given a
// listing's capacity, price, and condition, it computes the listing's price
// per TB and compares it against a category-reference price-per-TB to yield a
// categorical deal-quality verdict (docs/design section 9: "category-reference
// preferred over gated eBay sold-comps").
//
// # Reference source survey (2026-07-01) and the v1 pick
//
// Four candidate reference sources were evaluated for HDD $/TB:
//
//  1. ServerPartDeals (Shopify storefront) `products.json` -- an OPEN,
//     unauthenticated feed every Shopify store exposes. Its catalog is
//     manufacturer-recertified enterprise drives with a stated 2-year
//     warranty, and the JSON schema (products[].variants[].price, title,
//     tags) is the well-documented, stable Shopify public schema -- not
//     vendor-bespoke. CHOSEN AS PRIMARY for v1: verifiable schema today,
//     reachable without an API key, and its refurb/recertified inventory
//     gives a trustworthy anchor tier (see condition discussion below).
//  2. PricePerGig / DatacenterDisk -- reported free JSON APIs for $/TB.
//     UNVERIFIED as of this writing: no confirmed schema, auth requirements,
//     or uptime guarantees were established during research. The
//     ReferenceSource interface below exists precisely so one of these can be
//     added later as a second implementation without touching the Valuer or
//     scoring logic. FLAGGED FOR LIVE CHECK before relying on it.
//  3. diskprices.com -- scrape-only (no JSON endpoint found); skipped for v1
//     per the "scrape backend is a v2 concern" design note.
//  4. eBay Browse API -- belongs to the eBay connector (a separate,
//     cross-listing-source integration), out of scope for a single-vendor
//     reference adapter; see repo boundary notes in docs/design section 9.
//
// Nothing in this file or in Valuer assumes a particular vendor's JSON shape:
// all vendor-specific decoding lives inside that vendor's ReferenceSource
// implementation (shopifySource below), isolated behind the ReferenceSource
// interface so sources can be swapped or added side-by-side.
package hdd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Condition tiers understood by this package. Item.Condition is a free
// category-defined string; callers should normalize to one of these before
// calling Valuer.Value, though unrecognized strings are handled gracefully
// (see resolveReference).
const (
	ConditionNew    = "new"
	ConditionRefurb = "refurb"
	ConditionUsed   = "used"
)

// Verdict is the categorical deal-quality signal produced by Valuer.Value.
type Verdict string

const (
	// VerdictGreat means the listing is well below the reference $/TB.
	VerdictGreat Verdict = "great"
	// VerdictGood means the listing is moderately below the reference $/TB.
	VerdictGood Verdict = "good"
	// VerdictMarket means the listing is roughly at the reference $/TB.
	VerdictMarket Verdict = "market"
	// VerdictPoor means the listing is above the reference $/TB.
	VerdictPoor Verdict = "poor"
	// VerdictUnknownNoReference means no category-reference $/TB could be
	// resolved (directly or via condition-tier fallback) for this
	// capacity/condition. Not an error: a first-class "we don't know" state.
	VerdictUnknownNoReference Verdict = "unknown-no-reference"
	// VerdictUnknownNoPrice means the listing's price is unknown (PriceCents
	// == 0, per item.Item's documented convention). Never divide by an
	// unknown price; this verdict short-circuits before any math.
	VerdictUnknownNoPrice Verdict = "unknown-no-price"
)

// ErrInvalidCapacity is returned when capacityTB is <= 0: a valuation cannot
// be computed without a positive capacity, and treating it as "unknown" would
// hide what is very likely an extraction bug upstream (capacity should always
// be present on an "hdd" category item by the time it reaches valuation).
var ErrInvalidCapacity = errors.New("hdd: capacity_tb must be > 0")

// ReferenceSource resolves a category-reference price-per-TB for a hard
// drive of a given capacity and condition tier. Implementations are free to
// use any backing data (a vendor feed, an aggregator API, a static table for
// tests, ...); Valuer never assumes a vendor-specific schema -- see the
// package doc for the source survey behind this design.
type ReferenceSource interface {
	// PricePerTB returns the reference price-per-TB, in integer cents, for a
	// drive of the given capacity (TB) and condition tier (ConditionNew,
	// ConditionRefurb, ConditionUsed, or another category-defined string the
	// source understands).
	//
	// ok=false means "no reference available for this capacity/condition" --
	// a first-class state, not an error (e.g. the source has no listings at
	// that capacity right now). err is reserved for transport/decode
	// failures (network error, non-2xx response, malformed payload, ...).
	PricePerTB(ctx context.Context, capacityTB float64, condition string) (referenceCentsPerTB int64, ok bool, err error)
}

// conditionMultiplier expresses a reference tier as a multiple of the refurb
// anchor price-per-TB, used by Valuer as a FALLBACK when a ReferenceSource
// has no direct data for the requested condition (this matters in practice:
// the v1 primary source, ServerPartDeals, is overwhelmingly a
// manufacturer-recertified refurb catalog, so "new" and "used" lookups will
// often miss directly).
//
// refurb is the anchor tier deliberately, not new or used: a stated
// manufacturer warranty term (e.g. ServerPartDeals' 2-year warranty) is the
// strongest TRUST SIGNAL available on a refurbished-drive listing, because it
// is a real, enforceable commitment, not a seller's unverifiable claim.
//
// By contrast, seller-reported power-on-hours (PoH) / SMART data are
// UNRELIABLE as a pricing input: SMART counters can be reset by a reseller,
// so a low-PoH claim on a listing cannot be treated as fact. nagus surfaces
// PoH to the human as a signal to check on arrival; it is deliberately NOT
// used anywhere in this package to adjust price expectations.
//
// These multipliers are v1 ESTIMATES documenting a reasonable spread between
// tiers pending real sale-price comps (not fit to data):
//
//	new:    refurb anchor * 1.35 -- unopened/manufacturer-new commands a
//	                                 premium over recertified for the same
//	                                 generation/capacity.
//	refurb: refurb anchor * 1.00 -- the anchor tier itself.
//	used:   refurb anchor * 0.75 -- used / seller-refurb without a
//	                                 manufacturer warranty trades at a
//	                                 discount that reflects the ABSENCE of
//	                                 the warranty trust signal -- not any
//	                                 claim about drive health.
var conditionMultiplier = map[string]float64{
	ConditionNew:    1.35,
	ConditionRefurb: 1.00,
	ConditionUsed:   0.75,
}

// Valuation is the result of comparing one listing's $/TB against the
// category reference.
type Valuation struct {
	ListingCentsPerTB int64 // listing price / capacity, rounded to the nearest cent

	ReferenceAvailable  bool   // false when no reference could be resolved at all
	ReferenceCentsPerTB int64  // reference $/TB in cents; 0 when !ReferenceAvailable
	ReferenceCondition  string // condition tier the reference reflects (may be derived, see ReferenceDerived)
	ReferenceDerived    bool   // true if ReferenceCentsPerTB came from the refurb-anchor condition-multiplier fallback rather than a direct source hit

	Ratio   float64 // ListingCentsPerTB / ReferenceCentsPerTB; 0 when !ReferenceAvailable
	Verdict Verdict
}

// Valuer computes Valuations for hdd-category listings against a
// ReferenceSource. The zero value is usable if Source is set; verdict
// thresholds default to the package defaults when left zero.
type Valuer struct {
	Source ReferenceSource

	// Ratio upper bounds for each non-poor verdict tier; a zero value in any
	// field falls back to the package default for that field. Ratio is
	// listing $/TB over reference $/TB, so lower is a better deal.
	GreatMaxRatio  float64 // default 0.80 (>=20% below reference)
	GoodMaxRatio   float64 // default 0.95 (>=5% below reference)
	MarketMaxRatio float64 // default 1.10 (within 10% above reference); above this is "poor"
}

const (
	defaultGreatMaxRatio  = 0.80
	defaultGoodMaxRatio   = 0.95
	defaultMarketMaxRatio = 1.10
)

func (v Valuer) thresholds() (great, good, market float64) {
	great, good, market = v.GreatMaxRatio, v.GoodMaxRatio, v.MarketMaxRatio
	if great <= 0 {
		great = defaultGreatMaxRatio
	}
	if good <= 0 {
		good = defaultGoodMaxRatio
	}
	if market <= 0 {
		market = defaultMarketMaxRatio
	}
	return great, good, market
}

// Value computes the Valuation for a listing with the given capacity (TB),
// price (integer minor-unit cents; 0 == unknown, per item.Item's documented
// convention), and condition tier.
//
// capacityTB <= 0 is an error (ErrInvalidCapacity): capacity should always be
// present and positive on an hdd-category item by the time it reaches
// valuation, so a non-positive value very likely indicates an upstream
// extraction bug and must not be silently swallowed into "unknown".
func (v Valuer) Value(ctx context.Context, capacityTB float64, priceCents int64, condition string) (Valuation, error) {
	if capacityTB <= 0 {
		return Valuation{}, ErrInvalidCapacity
	}
	if priceCents == 0 {
		// Unknown price: never divide. Short-circuit before touching the
		// reference source at all.
		return Valuation{Verdict: VerdictUnknownNoPrice}, nil
	}
	if priceCents < 0 {
		return Valuation{}, fmt.Errorf("hdd: price_cents must be >= 0, got %d", priceCents)
	}

	listingCentsPerTB := int64(math.Round(float64(priceCents) / capacityTB))

	refCentsPerTB, refCondition, derived, ok, err := v.resolveReference(ctx, capacityTB, condition)
	if err != nil {
		return Valuation{}, err
	}
	if !ok {
		return Valuation{
			ListingCentsPerTB: listingCentsPerTB,
			Verdict:           VerdictUnknownNoReference,
		}, nil
	}

	ratio := float64(listingCentsPerTB) / float64(refCentsPerTB)
	great, good, market := v.thresholds()

	var verdict Verdict
	switch {
	case ratio <= great:
		verdict = VerdictGreat
	case ratio <= good:
		verdict = VerdictGood
	case ratio <= market:
		verdict = VerdictMarket
	default:
		verdict = VerdictPoor
	}

	return Valuation{
		ListingCentsPerTB:   listingCentsPerTB,
		ReferenceAvailable:  true,
		ReferenceCentsPerTB: refCentsPerTB,
		ReferenceCondition:  refCondition,
		ReferenceDerived:    derived,
		Ratio:               ratio,
		Verdict:             verdict,
	}, nil
}

// resolveReference asks Source for a direct reference at the requested
// condition; if that misses (ok=false) it falls back to the refurb anchor
// tier and applies conditionMultiplier, per the fallback design documented on
// conditionMultiplier above. Returns ok=false only if neither the direct
// lookup nor the fallback produced a reference.
func (v Valuer) resolveReference(ctx context.Context, capacityTB float64, condition string) (centsPerTB int64, resolvedCondition string, derived bool, ok bool, err error) {
	cond := strings.ToLower(strings.TrimSpace(condition))

	direct, ok, err := v.Source.PricePerTB(ctx, capacityTB, cond)
	if err != nil {
		return 0, "", false, false, err
	}
	if ok {
		return direct, cond, false, true, nil
	}

	// Direct miss: fall back to the refurb anchor, if we know how to scale
	// it for the requested condition and it isn't refurb itself (already
	// tried above).
	if cond == ConditionRefurb {
		return 0, "", false, false, nil
	}
	mult, known := conditionMultiplier[cond]
	if !known {
		return 0, "", false, false, nil
	}
	anchor, ok, err := v.Source.PricePerTB(ctx, capacityTB, ConditionRefurb)
	if err != nil {
		return 0, "", false, false, err
	}
	if !ok {
		return 0, "", false, false, nil
	}
	return int64(math.Round(float64(anchor) * mult)), cond, true, true, nil
}

// --- ServerPartDeals-style Shopify products.json reference source ---
//
// Shopify's public, unauthenticated `products.json` endpoint is a stable,
// documented storefront schema (not vendor-bespoke), which is why it was
// picked as the v1 primary source -- see the package doc for the full
// survey. All vendor-shaped decoding is isolated below (shopifyProduct /
// shopifyVariant / shopifyTags) so it cannot leak into Valuer's scoring
// logic, and so a second ReferenceSource (e.g. an eventual PricePerGig /
// DatacenterDisk client) can be added without touching this type.

// shopifyTags accepts either JSON encoding Shopify storefronts have used for
// the product "tags" field: a JSON array of strings, or a single
// comma-separated string. Isolating this quirk here keeps the rest of the
// decode path simple.
type shopifyTags []string

func (t *shopifyTags) UnmarshalJSON(data []byte) error {
	var asArray []string
	if err := json.Unmarshal(data, &asArray); err == nil {
		*t = asArray
		return nil
	}
	var asString string
	if err := json.Unmarshal(data, &asString); err != nil {
		return err
	}
	if asString == "" {
		*t = nil
		return nil
	}
	parts := strings.Split(asString, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	*t = parts
	return nil
}

type shopifyVariant struct {
	Title     string `json:"title"`
	Price     string `json:"price"` // Shopify encodes price as a decimal string, e.g. "179.99"
	Available bool   `json:"available"`
	SKU       string `json:"sku"`
}

type shopifyProduct struct {
	Title       string           `json:"title"`
	ProductType string           `json:"product_type"`
	Tags        shopifyTags      `json:"tags"`
	Variants    []shopifyVariant `json:"variants"`
}

type shopifyProductsResponse struct {
	Products []shopifyProduct `json:"products"`
}

// offer is the vendor-agnostic normalized record shopifySource builds from
// the raw Shopify payload: one per (product, variant) pair with a resolved
// capacity, condition, and $/TB. Everything downstream of parseShopify
// operates only on offer, never on the raw shopify* types.
type offer struct {
	capacityTB float64
	condition  string
	centsPerTB int64
}

var capacityTBPattern = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*TB\b`)

// parseCapacityTB extracts a capacity in TB from free text (a Shopify
// product/variant title) by matching the first "<number>TB" token. This is a
// heuristic over untrusted-ish vendor text, not a guarantee; callers should
// treat a false return as "could not determine capacity" rather than assume
// zero.
func parseCapacityTB(text string) (float64, bool) {
	m := capacityTBPattern.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	tb, err := strconv.ParseFloat(m[1], 64)
	if err != nil || tb <= 0 {
		return 0, false
	}
	return tb, true
}

// inferCondition guesses a condition tier from a Shopify product's title and
// tags. ServerPartDeals' catalog defaults to manufacturer-recertified refurb
// stock, so that is the fallback when no explicit condition keyword is
// present -- consistent with refurb being the anchor tier documented on
// conditionMultiplier above.
func inferCondition(title string, tags []string) string {
	hay := strings.ToLower(title)
	for _, t := range tags {
		hay += " " + strings.ToLower(t)
	}
	switch {
	case strings.Contains(hay, "recertified"), strings.Contains(hay, "refurb"), strings.Contains(hay, "recert"):
		return ConditionRefurb
	case strings.Contains(hay, "used"), strings.Contains(hay, "pull"):
		return ConditionUsed
	case strings.Contains(hay, "new"):
		return ConditionNew
	default:
		return ConditionRefurb
	}
}

// parsePriceCents parses a Shopify decimal price string ("179.99") into
// integer cents.
func parsePriceCents(price string) (int64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(price), 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return int64(math.Round(f * 100)), true
}

// capacityBucket rounds a capacity to one decimal place so that
// floating-point noise (e.g. 16.0 vs 15.999999) does not prevent a listing
// from matching a reference offer at the "same" nominal capacity.
func capacityBucket(tb float64) float64 {
	return math.Round(tb*10) / 10
}

// ShopifySource is a ReferenceSource backed by a Shopify storefront's public
// `products.json` feed (e.g. ServerPartDeals). See the package doc for why
// this was chosen as the v1 primary reference source.
type ShopifySource struct {
	// ProductsURL is the full products.json URL, e.g.
	// "https://www.serverpartdeals.com/products.json?limit=250".
	ProductsURL string
	// HTTPClient is used for the fetch; if nil, http.DefaultClient is used.
	HTTPClient *http.Client
	// CacheTTL, if > 0, caches the parsed offer list for this long between
	// fetches. Zero means "fetch fresh every call" (the default), which is
	// simplest and correct but calls the vendor endpoint once per
	// PricePerTB call; production callers likely want to set this.
	CacheTTL time.Duration
	// Now, if set, is used instead of time.Now for cache-expiry checks
	// (test seam). Defaults to time.Now.
	Now func() time.Time

	mu         sync.Mutex
	cached     []offer
	cachedAt   time.Time
	cacheValid bool
}

var _ ReferenceSource = (*ShopifySource)(nil)

func (s *ShopifySource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *ShopifySource) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return http.DefaultClient
}

// PricePerTB implements ReferenceSource. It fetches (or reuses a cached copy
// of) the products.json feed, then returns the median $/TB among offers
// matching capacityTB (rounded to the nearest 0.1 TB) and the given
// condition tier.
func (s *ShopifySource) PricePerTB(ctx context.Context, capacityTB float64, condition string) (int64, bool, error) {
	if capacityTB <= 0 {
		return 0, false, ErrInvalidCapacity
	}
	offers, err := s.offers(ctx)
	if err != nil {
		return 0, false, err
	}

	wantCapacity := capacityBucket(capacityTB)
	wantCondition := strings.ToLower(strings.TrimSpace(condition))

	var matched []int64
	for _, o := range offers {
		if capacityBucket(o.capacityTB) != wantCapacity {
			continue
		}
		if o.condition != wantCondition {
			continue
		}
		matched = append(matched, o.centsPerTB)
	}
	if len(matched) == 0 {
		return 0, false, nil
	}
	return median(matched), true, nil
}

func median(vals []int64) int64 {
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return int64(math.Round(float64(sorted[n/2-1]+sorted[n/2]) / 2))
}

func (s *ShopifySource) offers(ctx context.Context) ([]offer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.CacheTTL > 0 && s.cacheValid && s.now().Sub(s.cachedAt) < s.CacheTTL {
		return s.cached, nil
	}

	fetched, err := s.fetch(ctx)
	if err != nil {
		return nil, err
	}
	s.cached = fetched
	s.cachedAt = s.now()
	s.cacheValid = true
	return fetched, nil
}

func (s *ShopifySource) fetch(ctx context.Context) ([]offer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.ProductsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("hdd: building products.json request: %w", err)
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("hdd: fetching products.json: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hdd: products.json returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hdd: reading products.json body: %w", err)
	}

	var parsed shopifyProductsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("hdd: decoding products.json: %w", err)
	}

	return offersFromShopify(parsed), nil
}

// offersFromShopify maps the raw Shopify payload into normalized offers.
// Isolated as a pure function (no I/O) so the mapping logic is testable
// independent of HTTP.
func offersFromShopify(parsed shopifyProductsResponse) []offer {
	var out []offer
	for _, p := range parsed.Products {
		condition := inferCondition(p.Title, p.Tags)
		for _, v := range p.Variants {
			if !v.Available {
				continue
			}
			capacityTB, ok := parseCapacityTB(v.Title)
			if !ok {
				capacityTB, ok = parseCapacityTB(p.Title)
			}
			if !ok {
				continue
			}
			priceCents, ok := parsePriceCents(v.Price)
			if !ok || priceCents <= 0 {
				continue
			}
			out = append(out, offer{
				capacityTB: capacityTB,
				condition:  condition,
				centsPerTB: int64(math.Round(float64(priceCents) / capacityTB)),
			})
		}
	}
	return out
}
