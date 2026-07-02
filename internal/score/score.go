// Package score implements the two deterministic pipeline stages that sit
// between STORE and SURFACE in the nagus spine (docs/design section 4):
//
//	STORE -> HARD-FILTER -> ENRICH -> SCORE -> SURFACE
//
// Hard-filter is cheap, deterministic, has no I/O, and runs BEFORE any paid
// enrichment call -- its whole purpose is to bound enrichment/valuation API
// volume to survivors (design section 4, "hard-filter before enrich" is a
// first-class design constraint, not an optimization). Scoring runs only on
// survivors and ranks them using a category-agnostic deal signal that an
// enrichment/valuation adapter (e.g. internal/valuation/hdd) is expected to
// produce.
//
// This package is deliberately category-generic: it is config-driven (Filter
// is data, not code) and it never imports a category valuation package. A
// category bundle (design section 5) supplies a Filter fill and maps its own
// valuation output onto DealSignal; score.go does not know "hdd" or "land"
// exist.
package score

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/leftathome/nagus/internal/item"
)

// Filter is a deterministic, config-driven hard-filter for one category.
// Every predicate is optional; the zero value of a field means "no
// constraint" (an empty Filter{} passes every item, priced or not, since
// RequirePriced defaults to false).
//
// Filter.Pass must never call out to enrichment/valuation (no network, no
// I/O) -- it exists solely to bound paid-API volume to the survivors that
// make it through this stage. Anything that requires a paid lookup belongs
// in ENRICH, not here.
type Filter struct {
	// Category, if non-empty, requires item.Category to match exactly.
	Category string

	// RequirePriced, if true, fails any item with PriceCents <= 0 (item.Item
	// documents 0 as "unknown", never "free").
	RequirePriced bool

	// MaxPriceCents, if > 0, fails any *known-priced* item over this bound.
	// An unpriced item (PriceCents <= 0) is never failed by this predicate on
	// its own -- pair with RequirePriced to also reject unpriced items.
	MaxPriceCents int64

	// MinAttr/MaxAttr bound numeric-valued item.Attributes. Attribute values
	// are strings in item.Item; each is parsed with strconv.ParseFloat. A
	// required attribute that is missing, or present but not parseable as a
	// number, FAILS the filter with a reason naming the attribute -- it is
	// never silently skipped, since a missing required signal is exactly the
	// kind of thing an ingestion log needs to explain.
	MinAttr map[string]float64
	MaxAttr map[string]float64

	// AllowedConditions, if non-empty, requires item.Condition to be one of
	// the listed values (exact string match; condition vocabulary is
	// category-defined per design section 5).
	AllowedConditions []string
}

// Pass reports whether it survives the filter. On failure it also returns a
// short, human-readable reason identifying which predicate rejected the item,
// so ingestion logs can explain drops without re-deriving the logic.
//
// Predicates are checked in a fixed order (category, priced, max price, min
// attrs, max attrs, condition) so the reason for a given Filter+Item pair is
// deterministic and stable across runs.
func (f Filter) Pass(it item.Item) (bool, string) {
	if f.Category != "" && it.Category != f.Category {
		return false, fmt.Sprintf("category %q does not match filter category %q", it.Category, f.Category)
	}

	if f.RequirePriced && it.PriceCents <= 0 {
		return false, "price unknown (price_cents <= 0) but filter requires a priced listing"
	}

	// A price of 0 means "unknown", not "free" (item.Item convention); only
	// compare known prices against the max bound.
	if f.MaxPriceCents > 0 && it.PriceCents > 0 && it.PriceCents > f.MaxPriceCents {
		return false, fmt.Sprintf("price %d cents exceeds max %d cents", it.PriceCents, f.MaxPriceCents)
	}

	if ok, reason := checkMin(it, f.MinAttr); !ok {
		return false, reason
	}
	if ok, reason := checkMax(it, f.MaxAttr); !ok {
		return false, reason
	}

	if len(f.AllowedConditions) > 0 && !containsString(f.AllowedConditions, it.Condition) {
		return false, fmt.Sprintf("condition %q not in allowed list %v", it.Condition, f.AllowedConditions)
	}

	return true, ""
}

// checkMin validates every bound in mins against it.Attributes in
// deterministic (sorted) key order, so the reason string is stable when
// multiple attributes are out of bounds.
func checkMin(it item.Item, mins map[string]float64) (bool, string) {
	for _, name := range sortedKeys(mins) {
		min := mins[name]
		val, ok, reason := parseAttr(it, name, min)
		if !ok {
			return false, reason
		}
		if val < min {
			return false, fmt.Sprintf("attribute %q value %v is below minimum %v", name, val, min)
		}
	}
	return true, ""
}

func checkMax(it item.Item, maxes map[string]float64) (bool, string) {
	for _, name := range sortedKeys(maxes) {
		max := maxes[name]
		val, ok, reason := parseAttr(it, name, max)
		if !ok {
			return false, reason
		}
		if val > max {
			return false, fmt.Sprintf("attribute %q value %v is above maximum %v", name, val, max)
		}
	}
	return true, ""
}

// parseAttr fetches and parses a required numeric attribute. A missing or
// unparseable attribute is a filter failure, not a skip: the caller must
// report why so the drop is explainable.
func parseAttr(it item.Item, name string, bound float64) (float64, bool, string) {
	raw, present := it.Attributes[name]
	if !present {
		return 0, false, fmt.Sprintf("attribute %q is required (bound %v) but missing", name, bound)
	}
	val, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false, fmt.Sprintf("attribute %q value %q is not numeric (bound %v)", name, raw, bound)
	}
	return val, true, ""
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// DealSignal is the category-agnostic deal-quality signal that a valuation /
// enrichment adapter produces for one item (design section 9-10: category
// valuation compares a listing metric, e.g. $/TB, against a category
// reference). score never computes this itself and never imports a
// valuation package -- the pipeline is responsible for mapping its adapter's
// output (see e.g. internal/valuation/hdd.Valuation) onto this struct.
type DealSignal struct {
	// Verdict is a category-agnostic label. The taxonomy this package
	// understands is: "great", "good", "market", "poor",
	// "unknown-no-reference" (no category reference could be resolved),
	// "unknown-no-price" (listing price itself is unknown). Any other string
	// is treated as unranked/unrecognized so the pipeline degrades gracefully
	// rather than panicking on a new adapter's verdict vocabulary.
	Verdict string

	// Ratio is listing-metric / reference-metric (e.g. listing $/TB over
	// reference $/TB). Lower is a better deal. Only meaningful when
	// HasReference is true.
	Ratio float64

	// HasReference reports whether Ratio was computed against a resolved
	// category reference. When false, Ratio is ignored by ScoreItem.
	HasReference bool
}

// Verdict string constants understood by ScoreItem. These intentionally
// mirror the Verdict vocabulary emitted by category valuation adapters (see
// internal/valuation/hdd.Verdict) as plain strings, without importing that
// package -- score stays decoupled from any one category.
const (
	VerdictGreat              = "great"
	VerdictGood               = "good"
	VerdictMarket             = "market"
	VerdictPoor               = "poor"
	VerdictUnknownNoReference = "unknown-no-reference"
	VerdictUnknownNoPrice     = "unknown-no-price"
)

// Tier names produced by ScoreItem.
const (
	TierGreat    = "great"
	TierGood     = "good"
	TierMarket   = "market"
	TierPoor     = "poor"
	TierUnranked = "unranked"
)

// base values per tier. Spacing (25) is chosen so that ratioDelta's clamp
// range (+/- maxRatioDelta, well under half the spacing) can never let a
// refined score in one tier cross into an adjacent tier's base range.
const (
	baseGreat            = 100.0
	baseGood             = 75.0
	baseMarket           = 50.0
	basePoor             = 25.0
	baseUnknownReference = 10.0 // saw the item, valuation just couldn't place it
	baseUnknownPrice     = 0.0  // can't even tell if it's a deal; ranks lowest
	baseUnrecognized     = 0.0  // unknown verdict vocabulary; treat conservatively
)

const (
	ratioDeltaScale = 5.0 // weight applied to (1 - Ratio)
	maxRatioDelta   = 5.0 // clamp so tiers (spaced 25 apart) never invert
)

// Score is the deterministic v1 output of ScoreItem: a sortable Value, the
// human-facing Tier it landed in, and a short Rationale explaining why.
//
// v1 scoring is entirely deterministic (verdict -> tier/base, refined by
// Ratio). Design section 10 calls for scoring to end with an LLM ranking the
// short list of survivors -- that step is intentionally NOT here. ScoreItem
// is the seam: it produces the deterministic gate/ordering that bounds what
// an LLM would rank next; nothing in this package calls an LLM.
type Score struct {
	Value     float64
	Tier      string
	Rationale string
}

// ScoreItem computes a deterministic Score for a filter-survivor it, given
// the deal signal an enrichment/valuation adapter produced for it. It never
// performs I/O and never calls an LLM (see the Score doc comment for the LLM
// seam this leaves for a later stage).
//
// Verdict maps to a base Tier/Value (great > good > market > poor; the two
// unknown-* verdicts get a low/neutral Value and Tier "unranked", with
// unknown-no-price ranking below unknown-no-reference since an unpriced item
// carries even less signal). When HasReference is true, Value is refined by
// Ratio within the tier (lower Ratio, i.e. cheaper relative to the category
// reference, refines upward) so items sort sensibly within a tier instead of
// tying.
func ScoreItem(it item.Item, sig DealSignal) Score {
	var base float64
	var tier string

	switch sig.Verdict {
	case VerdictGreat:
		base, tier = baseGreat, TierGreat
	case VerdictGood:
		base, tier = baseGood, TierGood
	case VerdictMarket:
		base, tier = baseMarket, TierMarket
	case VerdictPoor:
		base, tier = basePoor, TierPoor
	case VerdictUnknownNoReference:
		base, tier = baseUnknownReference, TierUnranked
	case VerdictUnknownNoPrice:
		base, tier = baseUnknownPrice, TierUnranked
		// Price is unknown: Ratio cannot be meaningful even if HasReference
		// were somehow set, so skip refinement entirely for this verdict.
		return Score{
			Value:     base,
			Tier:      tier,
			Rationale: "unknown-no-price: listing price unknown, cannot assess deal quality",
		}
	default:
		base, tier = baseUnrecognized, TierUnranked
		return Score{
			Value:     base,
			Tier:      tier,
			Rationale: fmt.Sprintf("unrecognized verdict %q: treated as unranked", sig.Verdict),
		}
	}

	value := base
	rationale := fmt.Sprintf("%s: no category reference available", sig.Verdict)

	if sig.HasReference {
		delta := ratioDeltaScale * (1 - sig.Ratio)
		if delta > maxRatioDelta {
			delta = maxRatioDelta
		}
		if delta < -maxRatioDelta {
			delta = -maxRatioDelta
		}
		value = base + delta
		rationale = fmt.Sprintf("%s: %.2fx reference (value %.1f)", sig.Verdict, sig.Ratio, value)
	}

	return Score{Value: value, Tier: tier, Rationale: rationale}
}

// Ranked pairs an item with its Score, ready to sort/surface.
type Ranked struct {
	Item  item.Item
	Score Score
}

// Rank returns rs sorted best-first by Score.Value descending. Ties break
// deterministically by ascending item.ID so repeated runs over the same
// input produce an identical order (required for reproducible surfacing /
// tests). Rank does not mutate rs; it returns a new, sorted slice.
func Rank(rs []Ranked) []Ranked {
	out := make([]Ranked, len(rs))
	copy(out, rs)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score.Value != out[j].Score.Value {
			return out[i].Score.Value > out[j].Score.Value
		}
		return out[i].Item.ID < out[j].Item.ID
	})
	return out
}
