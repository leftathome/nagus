// Package hdd implements the "hdd" category listing.Extractor: it lifts a
// glovebox-sanitized listing.Sanitized into a normalized item.Item using only
// deterministic regex/dictionary rules (no LLM in v1).
//
// This is the injection-containment stage described in docs/design section 7
// ("sanitize + extract/tokenize"): the extractor's output is a CONSTRAINED
// TYPED SCHEMA (item.Item, with a fixed set of typed fields and a string ->
// string Attributes map). Even though the input Title has already crossed the
// glovebox gate and is trusted-as-data, this package treats it as free text
// that is only ever read by regex/keyword matching -- never interpreted,
// never sent to an LLM instruction context, never used to control program
// flow beyond "did this pattern match." The worst a malicious or malformed
// listing can do here is produce a wrong (or absent) field value; it cannot
// hijack extraction. Title is also carried through to item.Item.Title
// verbatim, again as inert data for downstream FTS/display, not as
// instructions.
package hdd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
)

// Extractor implements listing.Extractor for category "hdd".
type Extractor struct{}

var _ listing.Extractor = (*Extractor)(nil)

// New returns an Extractor for the "hdd" category.
func New() *Extractor {
	return &Extractor{}
}

// Category returns "hdd".
func (e *Extractor) Category() string {
	return "hdd"
}

// Extract normalizes one sanitized HDD listing into an item.Item. It returns
// an error only when the resulting item would fail item.Validate (e.g. an
// empty SourceKey) or the price is negative -- i.e. when no valid item of
// this category can be formed. A missing/unparseable capacity or condition is
// NOT an error; those attributes are simply left unset, and the downstream
// hard-filter stage is responsible for enforcing and explaining any capacity
// requirement.
func (e *Extractor) Extract(_ context.Context, s listing.Sanitized) (item.Item, error) {
	it := item.Item{
		ID:          deterministicID(s.SourceID, s.SourceKey),
		Category:    "hdd",
		Class:       item.ClassDurable,
		Title:       s.Title, // untrusted-as-data: carried verbatim, never interpreted
		CanonicalID: extractCanonicalID(s.Title),
		PriceCents:  s.PriceCents,
		Currency:    s.Currency,
		Condition:   extractCondition(s.ConditionRaw, s.Title),
		SourceID:    s.SourceID,
		SourceKey:   s.SourceKey,
		SourceURL:   s.SourceURL,
		SeenAt:      s.SeenAt,
		Attributes:  map[string]string{},
		Tokens:      tokenize(s.Title),
	}

	if tb, ok := extractCapacityTB(s.Title); ok {
		it.Attributes["capacity_tb"] = tb
	}

	// Seller trust signals: coarse, non-identifying buckets derived from eBay's
	// PUBLIC seller data (surfaced by the connector as aspects). Only the bucket
	// is stored, never the raw percentage/count or the seller username -- so the
	// persisted item carries no eBay user PII and nagus qualifies for the
	// Marketplace Account Deletion opt-out. See SECURITY.md.
	if tier, ok := sellerFeedbackTier(s.Aspects["seller_feedback_pct"]); ok {
		it.Attributes["seller_feedback_tier"] = tier
	}
	if tier, ok := sellerVolumeTier(s.Aspects["seller_feedback_score"]); ok {
		it.Attributes["seller_volume_tier"] = tier
	}
	if v, ok := shipsFromUS(s.Aspects["item_location_country"]); ok {
		it.Attributes["ships_from_us"] = v
	}

	if err := it.Validate(); err != nil {
		return item.Item{}, fmt.Errorf("hdd: extract: %w", err)
	}
	return it, nil
}

// deterministicID derives a stable nagus id from a source's identity and its
// source-native key, so the same listing always maps to the same id (the
// store's dedup/identity key) and different listings (almost certainly) do
// not collide. It is a truncated hex sha256 over "<sourceID>\x00<sourceKey>";
// the NUL separator avoids ambiguity between e.g. sourceID "eb" + sourceKey
// "ay123" and sourceID "ebay" + sourceKey "123".
func deterministicID(sourceID, sourceKey string) string {
	sum := sha256.Sum256([]byte(sourceID + "\x00" + sourceKey))
	return hex.EncodeToString(sum[:])[:16]
}

// capacityRe matches a number immediately (optionally space-separated)
// followed by a capacity unit, TB or GB. It deliberately requires the unit to
// be spelled with both letters in the same case (TB/tb/GB/gb) -- NOT a mixed
// case like "Gb" -- because storage capacity is conventionally expressed in
// bytes (a capital B) while transfer-rate specs on the same listing (e.g.
// "6Gb/s SATA") are conventionally expressed in bits (a lowercase b after an
// uppercase rate letter). This keeps a drive's SATA/SAS link speed from being
// misread as its capacity.
var capacityRe = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(TB|tb|GB|gb)\b`)

// extractCapacityTB scans title for a capacity expressed in TB or GB and
// returns it normalized to TB as a string with no trailing zeros. It reports
// ok=false (and sets no attribute) when no capacity pattern is found -- that
// is not an error, just an absent fact.
func extractCapacityTB(title string) (string, bool) {
	m := capacityRe.FindStringSubmatch(title)
	if m == nil {
		return "", false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return "", false
	}
	unit := strings.ToUpper(m[2])
	if unit == "GB" {
		v /= 1000
	}
	// strconv's shortest ('f', -1) representation drops trailing zeros, e.g.
	// 16.0 -> "16", 14.0 -> "14", 8000/1000=8.0 -> "8".
	return strconv.FormatFloat(v, 'f', -1, 64), true
}

// sellerFeedbackTier maps eBay's public seller.feedbackPercentage (a 0-100
// string, e.g. "99.4") to a COARSE trust bucket. It returns ok=false (and no
// attribute is written) when the value is absent or unparseable. Only the
// bucket is ever stored -- never the raw percentage -- so the persisted item
// carries no seller-identifying granularity. Buckets: high >=99.0, good >=97.0,
// mixed >=90.0, low <90.0.
func sellerFeedbackTier(pct string) (string, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(pct), 64)
	if err != nil {
		return "", false
	}
	switch {
	case v >= 99.0:
		return "high", true
	case v >= 97.0:
		return "good", true
	case v >= 90.0:
		return "mixed", true
	default:
		return "low", true
	}
}

// sellerVolumeTier maps eBay's public seller.feedbackScore (a lifetime feedback
// count, a proxy for sales volume) to a coarse bucket. ok=false when absent or
// unparseable. Only the bucket is stored, never the raw count. Buckets:
// established >=1000, mid >=100, new otherwise.
func sellerVolumeTier(score string) (string, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(score))
	if err != nil {
		return "", false
	}
	switch {
	case n >= 1000:
		return "established", true
	case n >= 100:
		return "mid", true
	default:
		return "new", true
	}
}

// shipsFromUS reports whether the listing's item-location country is the US, as
// a boolean-string attribute. This is the LISTING's ship-from location (from
// itemLocation.country), not the seller's registration country, and is coarse
// by construction. ok=false when the country is absent, so absence stays
// distinct from a known non-US origin.
func shipsFromUS(country string) (string, bool) {
	c := strings.TrimSpace(country)
	if c == "" {
		return "", false
	}
	if c == "US" {
		return "true", true
	}
	return "false", true
}

// modelRes holds best-effort manufacturer model-number patterns, tried in
// order; the first match wins. These are best-effort only -- CanonicalID is
// left "" when nothing matches, which is not an error.
var modelRes = []*regexp.Regexp{
	regexp.MustCompile(`\bST\d{4,5}[A-Z0-9]+\b`), // Seagate, e.g. ST16000NM001G
	regexp.MustCompile(`\bWUH\d+\b`),             // WD Ultrastar He, e.g. WUH721816ALE6L4
	regexp.MustCompile(`\bWD\d+\b`),              // WD, e.g. WD140EDGZ
}

// extractCanonicalID best-effort scans title for a manufacturer model number.
func extractCanonicalID(title string) string {
	for _, re := range modelRes {
		if m := re.FindString(title); m != "" {
			return m
		}
	}
	return ""
}

// conditionByID maps eBay's numeric conditionId enum to nagus's normalized
// condition vocabulary. 1500 (open box) is deliberately mapped to "new": an
// open-box item is unused/sealed-adjacent, so it is treated as new-ish rather
// than given its own bucket in v1 -- documented here as a conscious choice,
// not an oversight. 2000/2010/2500 (manufacturer / certified / seller
// refurbished) all collapse to "refurb": nagus does not yet distinguish
// refurbisher provenance.
var conditionByID = map[string]string{
	"1000": "new",    // New
	"1500": "new",    // New other (open box) -- treated as new-ish, see above
	"2000": "refurb", // Certified refurbished
	"2010": "refurb", // Excellent refurbished / manufacturer refurbished
	"2500": "refurb", // Seller refurbished
	"3000": "used",   // Used
	"7000": "parts",  // For parts or not working
}

// conditionKeywords is the Title fallback scan, used only when ConditionRaw
// is empty or not a recognized conditionId. Checked in this order:
// parts/as-is is the strongest, most specific signal and is checked first so
// it is not shadowed by an incidental "new"/"used" elsewhere in the title;
// refurb keywords next; then new; then used.
var conditionKeywords = []struct {
	condition string
	keywords  []string
}{
	{"parts", []string{"for parts", "as-is", "as is"}},
	{"refurb", []string{"renewed", "refurbished", "recertified", "manufacturer recertified"}},
	{"new", []string{"brand new", "new"}},
	{"used", []string{"used"}},
}

// extractCondition normalizes condition from the trusted ConditionRaw source
// code first, falling back to a keyword scan of the (sanitized, treated as
// data) Title when ConditionRaw is empty or unrecognized. It returns "" when
// nothing matches -- absence, not an error.
func extractCondition(conditionRaw, title string) string {
	if c, ok := conditionByID[strings.TrimSpace(conditionRaw)]; ok {
		return c
	}
	lower := strings.ToLower(title)
	for _, ck := range conditionKeywords {
		for _, kw := range ck.keywords {
			if strings.Contains(lower, kw) {
				return ck.condition
			}
		}
	}
	return ""
}

// tokenRe splits a title into raw token candidates on anything that is not an
// ASCII letter or digit.
var tokenRe = regexp.MustCompile(`[^a-z0-9]+`)

// tokenize lowercases title, splits it on non-alphanumeric runs, drops empty
// and single-character tokens, and dedupes while preserving first-seen order.
// This feeds the store's FTS/text search; title is treated purely as data --
// tokenize never does anything but split and compare bytes.
func tokenize(title string) []string {
	lower := strings.ToLower(title)
	parts := tokenRe.Split(lower, -1)

	seen := make(map[string]bool, len(parts))
	tokens := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) < 2 {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		tokens = append(tokens, p)
	}
	return tokens
}
