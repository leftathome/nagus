package category

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/nagus/internal/enrich/geo"
	"github.com/leftathome/nagus/internal/enrich/parcel"
	extland "github.com/leftathome/nagus/internal/extract/land"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
)

// The land category scores parcels STRUCTURE-FIRST (operator choice): a present
// structure on land-value-dominant acreage is the top signal (the rural-fixer /
// inherited-entitlement case, design section 9 -- short permit path), rewarded
// when flood risk is low and the price fits; flood (AE/VE...) or wetlands
// downgrade regardless. Land is not a $/metric deal like HDD, so the Valuate
// hook runs geocode + the free gov geo enrichers + a parcel lookup on
// hard-filter survivors (filter-before-enrich) and maps the signals to a
// verdict the generic scorer ranks.

// DefaultMinAcreageAcres is the land hard-filter floor: it drops tiny/urban lots
// and unparseable-acreage noise, and -- because it runs BEFORE enrichment --
// bounds paid parcel-API volume to plausible parcels.
const DefaultMinAcreageAcres = 1.0

// LandScoreConfig tunes the structure-first rubric. Zero values take defaults
// (MinAcreageAcres) or mean "no constraint" (BudgetCents 0, MaxAcreageAcres 0).
type LandScoreConfig struct {
	BudgetCents     int64   // price ceiling; 0 = no budget (any priced listing fits)
	MinAcreageAcres float64 // hard-filter floor + scoring lower bound
	MaxAcreageAcres float64 // scoring upper bound; 0 = no max
}

func (c LandScoreConfig) minAcreage() float64 {
	if c.MinAcreageAcres > 0 {
		return c.MinAcreageAcres
	}
	return DefaultMinAcreageAcres
}

// geoEnricher is the subset of *geo.Enricher the land bundle needs, so tests can
// inject fakes without hitting the free gov APIs.
type geoEnricher interface {
	Geocode(ctx context.Context, address string) (lat, lon float64, err error)
	Enrich(ctx context.Context, lat, lon float64) (geo.Result, error)
}

// LandDeps are the injectable dependencies of the land bundle.
type LandDeps struct {
	Store store.Store
	// Geo resolves free gov geo signals; nil -> a live geo.NewEnricher.
	Geo geoEnricher
	// Parcel resolves structure/assessed-value/acreage; nil -> no parcel signals
	// (structure can then never be confirmed, so nothing scores "great"). A live
	// deployment injects a keyed parcel.NewRentcastProvider.
	Parcel     parcel.Provider
	HTTPClient *http.Client
	Score      LandScoreConfig
	Logf       func(format string, args ...any)
	// --- per-source retention (nagus-q6u) ---
	// StaleAfter, when > 0, purges this SOURCE's items older than the window.
	// It is a property of the source's terms (eBay License 8.1(b) needs 6h;
	// a Shopify storefront needs none), not of the category.
	StaleAfter time.Duration
	// Offers, when non-nil, enables the additive offer layer for this source.
	Offers offer.Store
	// OfferRetention is this source's offer retention policy.
	OfferRetention offer.Retention
	// OfferExpireAfter marks unseen offers expired (retained, not deleted).
	OfferExpireAfter time.Duration
}

// LandFilter is the deterministic hard-filter for land: category match plus the
// acreage floor (which also bounds enrichment cost). Price is NOT bounded here
// -- unpriced by-owner land is common and must still surface as a candidate.
func LandFilter(cfg LandScoreConfig) score.Filter {
	return score.Filter{
		Category: "land",
		MinAttr:  map[string]float64{"acreage": cfg.minAcreage()},
	}
}

// NewLandSurface builds the land surface (read half): acreage hard-filter +
// structure-first enrichment/scoring.
func NewLandSurface(deps LandDeps) *pipeline.Surface {
	geoE := deps.Geo
	if geoE == nil {
		geoE = geo.NewEnricher(deps.HTTPClient)
	}
	cfg := deps.Score
	valuate := func(ctx context.Context, it item.Item) (score.DealSignal, error) {
		sig, enriched := buildLandSignals(ctx, it, geoE, deps.Parcel, cfg)
		verdict := scoreLand(sig, it.PriceCents > 0, enriched)
		return score.DealSignal{Verdict: string(verdict), HasReference: enriched}, nil
	}
	return &pipeline.Surface{
		Store:   deps.Store,
		Filter:  LandFilter(cfg),
		Valuate: valuate,
		Logf:    deps.Logf,
	}
}

// NewLandIngester builds the land ingest half for one source connector. Land is
// not eBay Content, so no freshness purge (StaleAfter stays 0).
func NewLandIngester(conn listing.Connector, deps LandDeps) *pipeline.Ingester {
	return &pipeline.Ingester{
		Connector:        conn,
		Sanitizer:        sanitize.Passthrough{Name: "sanitize.passthrough(land)"},
		Extractor:        extland.New(),
		Store:            deps.Store,
		StaleAfter:       deps.StaleAfter,
		Offers:           deps.Offers,
		OfferRetention:   deps.OfferRetention,
		OfferExpireAfter: deps.OfferExpireAfter,
		Logf:             deps.Logf,
	}
}

// landSignals is the typed input to the structure-first rubric.
type landSignals struct {
	StructurePresent  bool
	LandValueDominant bool
	FloodLow          bool
	FloodHigh         bool
	Wetland           bool
	AcreageOK         bool
	PriceOK           bool
	FloodZone         string
}

// coordsFromAttrs reads exact per-listing coordinates an API source supplied.
// Both must parse and be non-zero: a 0,0 pair is null island, not a Washington
// parcel, and enriching there would produce a confident wrong answer.
func coordsFromAttrs(it item.Item) (lat, lon float64, ok bool) {
	la, laOK := parseFloatAttr(it, "lat")
	lo, loOK := parseFloatAttr(it, "lon")
	if !laOK || !loOK || la == 0 || lo == 0 {
		return 0, 0, false
	}
	if la < -90 || la > 90 || lo < -180 || lo > 180 {
		return 0, 0, false
	}
	return la, lo, true
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// buildLandSignals derives the rubric inputs. Typed fields (acreage, price) come
// from the item; structure/flood/wetland come from enrichment when a geocodable
// location is present. enriched reports whether any parcel/geo lookup resolved.
func buildLandSignals(ctx context.Context, it item.Item, geoE geoEnricher, parcelP parcel.Provider, cfg LandScoreConfig) (landSignals, bool) {
	var sig landSignals
	if ac, ok := parseFloatAttr(it, "acreage"); ok {
		sig.AcreageOK = ac >= cfg.minAcreage() && (cfg.MaxAcreageAcres <= 0 || ac <= cfg.MaxAcreageAcres)
	}
	sig.PriceOK = it.PriceCents > 0 && (cfg.BudgetCents <= 0 || it.PriceCents <= cfg.BudgetCents)

	// Positioning. These are deliberately DIFFERENT inputs, because they answer
	// different questions and using the wrong one is silently wrong rather than
	// visibly broken:
	//
	//   - geo enrichment (flood zone, wetlands) needs a POINT. A source that
	//     supplies exact per-listing coordinates is used directly; geocoding is
	//     only a fallback for sources that give a place name. Geocoding a CITY
	//     label -- which is what "location" is -- returns the city centroid, so
	//     the flood zone would describe downtown rather than the parcel.
	//   - a parcel lookup needs a STREET ADDRESS. "location" cannot resolve one.
	//
	// Absent any of them the item simply goes un-enriched, which is a valid
	// outcome the scorer already understands.
	locality := it.Attributes["location"]
	streetAddr := firstNonEmpty(it.Attributes["street_address"], locality)
	lat, lon, haveCoords := coordsFromAttrs(it)
	if !haveCoords && locality == "" {
		return sig, false
	}

	enriched := false
	if geoE != nil {
		if !haveCoords && locality != "" {
			if glat, glon, err := geoE.Geocode(ctx, locality); err == nil {
				lat, lon, haveCoords = glat, glon, true
			}
		}
		if haveCoords {
			enriched = true
			if gr, gerr := geoE.Enrich(ctx, lat, lon); gerr == nil {
				if gr.Flood != nil {
					sig.FloodZone = gr.Flood.Zone
				}
				if gr.Wetlands != nil && gr.Wetlands.Type != "" {
					sig.Wetland = true
				}
			}
		}
	}
	if parcelP != nil && streetAddr != "" {
		if pd, err := parcelP.Lookup(ctx, streetAddr); err == nil {
			enriched = true
			sig.StructurePresent = pd.StructurePresent()
			sig.LandValueDominant = pd.AssessedLandValueCents > 0 &&
				pd.AssessedLandValueCents >= pd.AssessedImprovementValueCents
		}
	}
	sig.FloodHigh = isFloodHigh(sig.FloodZone)
	sig.FloodLow = isFloodLow(sig.FloodZone)
	return sig, enriched
}

// scoreLand applies the structure-first rubric, returning a verdict string the
// generic scorer understands (the same taxonomy across categories). Precedence:
// unassessed -> risk red flags -> structure-led great/good -> buildable good ->
// market.
func scoreLand(sig landSignals, priceKnown, enriched bool) string {
	if !enriched {
		if priceKnown && sig.AcreageOK {
			return "market" // typed fit only; parcel/geo not resolvable from the listing
		}
		return "unknown-no-reference"
	}
	switch {
	case sig.FloodHigh:
		return "poor"
	case sig.Wetland:
		return "poor"
	case !priceKnown && !sig.StructurePresent:
		return "poor"
	case sig.StructurePresent && sig.LandValueDominant && sig.FloodLow && sig.PriceOK:
		return "great"
	case sig.StructurePresent:
		return "good"
	case sig.FloodLow && sig.AcreageOK && sig.PriceOK:
		return "good"
	default:
		return "market"
	}
}

// isFloodHigh reports whether an NFHL zone is a high-risk (Special Flood Hazard
// Area) designation.
func isFloodHigh(zone string) bool {
	switch zone {
	case "A", "AE", "AH", "AO", "AR", "A99", "V", "VE":
		return true
	}
	return false
}

// isFloodLow reports whether an NFHL zone is a confirmed low/moderate-risk
// designation (a great verdict requires this, not merely "not high").
func isFloodLow(zone string) bool {
	switch zone {
	case "X", "X500", "B", "C":
		return true
	}
	return false
}

func parseFloatAttr(it item.Item, key string) (float64, bool) {
	s, ok := it.Attributes[key]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
