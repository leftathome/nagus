// Package category assembles the generic spine (internal/pipeline) into concrete
// per-category bundles. A category is config + adapters over the spine (design
// section 5), so adding one is a bundle, not a new system. This file is the HDD
// reference bundle: the eBay-style connector is injected, the rest (sanitize
// boundary, HDD extractor, hard-filter, $/TB valuation, scoring) is wired here.
package category

import (
	"context"
	"net/http"
	"strconv"
	"time"

	exthdd "github.com/leftathome/nagus/internal/extract/hdd"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
	valhdd "github.com/leftathome/nagus/internal/valuation/hdd"
)

// DefaultReferenceProductsURL is the live category-reference $/TB feed used when
// no offline reference is supplied (ServerPartDeals' Shopify products.json, the
// recertified-refurb specialist chosen as the v1 primary source).
const DefaultReferenceProductsURL = "https://www.serverpartdeals.com/products.json?limit=250"

// DefaultMinCapacityTB is the hard-filter capacity floor: sub-consumer drives
// are not the deal-watch target, and dropping them cheaply bounds enrichment.
const DefaultMinCapacityTB = 6.0

// HDDDeps are the injectable dependencies of the HDD bundle. Zero-valued fields
// take sensible defaults (live reference source, DefaultMinCapacityTB).
type HDDDeps struct {
	Store store.Store
	// Reference resolves category-reference $/TB. When nil, a live
	// hdd.ShopifySource against DefaultReferenceProductsURL is used. The
	// vertical-slice proof injects StaticReference so it runs without network.
	Reference valhdd.ReferenceSource
	// HTTPClient is used only by the default live reference source.
	HTTPClient *http.Client
	// MinCapacityTB overrides DefaultMinCapacityTB when > 0.
	MinCapacityTB float64
	Logf          func(format string, args ...any)
}

// HDDFilter builds the deterministic hard-filter for the HDD category: priced,
// at or above the capacity floor. Exposed so the search CLI can reuse the exact
// same gate the ingest/surface path applies.
func HDDFilter(minCapacityTB float64) score.Filter {
	if minCapacityTB <= 0 {
		minCapacityTB = DefaultMinCapacityTB
	}
	return score.Filter{
		Category:      "hdd",
		RequirePriced: true,
		MinAttr:       map[string]float64{"capacity_tb": minCapacityTB},
	}
}

// EbayContentMaxAge is the freshness/retention window for eBay-sourced items.
// eBay License 8.1(b) requires displayed item listings to be no more than 6h
// older than the eBay Site and stored eBay Content to be deleted once no longer
// public; the post-ingest purge drops hdd items not re-seen within this window.
// Operators must set the ingest interval well below this so live listings are
// refreshed before they are purged.
const EbayContentMaxAge = 6 * time.Hour

// NewHDDPipeline wires the HDD bundle over the generic spine. conn may be nil
// for a surface-only pipeline (search): Ingest needs it, Surface does not.
func NewHDDPipeline(conn listing.Connector, deps HDDDeps) *pipeline.Pipeline {
	ref := deps.Reference
	if ref == nil {
		ref = &valhdd.ShopifySource{ProductsURL: DefaultReferenceProductsURL, HTTPClient: deps.HTTPClient}
	}
	valuer := valhdd.Valuer{Source: ref}

	valuate := func(ctx context.Context, it item.Item) (score.DealSignal, error) {
		capTB, ok := parseCapacityTB(it)
		if !ok {
			// Should not happen for hard-filter survivors, but never call the
			// valuer with an invalid capacity (it would error): degrade instead.
			return score.DealSignal{Verdict: string(valhdd.VerdictUnknownNoReference)}, nil
		}
		val, err := valuer.Value(ctx, capTB, it.PriceCents, it.Condition)
		if err != nil {
			return score.DealSignal{}, err
		}
		return score.DealSignal{
			Verdict:      string(val.Verdict),
			Ratio:        val.Ratio,
			HasReference: val.ReferenceAvailable,
		}, nil
	}

	return &pipeline.Pipeline{
		Connector: conn,
		Sanitizer: sanitize.Passthrough{Name: "sanitize.passthrough(hdd)"},
		Extractor: exthdd.New(),
		Store:     deps.Store,
		Filter:    HDDFilter(deps.MinCapacityTB),
		Valuate:   valuate,
		// eBay Content must not linger past its public life / 6h freshness bound.
		StaleAfter: EbayContentMaxAge,
		Logf:       deps.Logf,
	}
}

func parseCapacityTB(it item.Item) (float64, bool) {
	s, ok := it.Attributes["capacity_tb"]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// StaticReference is a fixed reference-$/TB source keyed by condition tier, for
// offline demos and the vertical-slice proof. Production uses the live
// hdd.ShopifySource. Capacity is ignored (a flat per-condition reference);
// Valuer's condition-tier fallback still applies when a tier is absent.
type StaticReference struct {
	CentsPerTB map[string]int64 // "new" | "refurb" | "used" -> reference cents/TB
}

var _ valhdd.ReferenceSource = StaticReference{}

// PricePerTB returns the fixed reference for the condition, or ok=false when the
// tier is absent (letting Valuer fall back through the refurb anchor).
func (s StaticReference) PricePerTB(_ context.Context, _ float64, condition string) (int64, bool, error) {
	v, ok := s.CentsPerTB[condition]
	if !ok {
		return 0, false, nil
	}
	return v, true, nil
}
