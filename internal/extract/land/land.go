// Package land implements the "land" category listing.Extractor: it lifts a
// glovebox-sanitized listing.Sanitized into a normalized item.Item using only
// deterministic regex/dictionary rules (no LLM in v1).
//
// This is the injection-containment stage described in docs/design section 7
// ("sanitize + extract/tokenize"): the extractor's output is a CONSTRAINED
// TYPED SCHEMA (item.Item, with a fixed set of typed fields and a string ->
// string Attributes map). Title and Body have already crossed the glovebox
// gate and are trusted-as-data, but this package treats them as inert free
// text that is only ever read by regex/keyword matching -- never
// interpreted, never sent to an LLM instruction context, never used to
// control program flow beyond "did this pattern match." Keyword flags
// (well, septic, fixer, manufactured, owner_financing) emit only typed
// "true" labels over that text. The worst a malicious or malformed listing
// can do here is produce a wrong (or absent) field value; it cannot hijack
// extraction. Title is also carried through to item.Item.Title verbatim,
// again as inert data for downstream FTS/display, not as instructions.
package land

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
)

// Extractor implements listing.Extractor for category "land".
type Extractor struct{}

var _ listing.Extractor = (*Extractor)(nil)

// New returns an Extractor for the "land" category.
func New() *Extractor {
	return &Extractor{}
}

// Category returns "land".
func (e *Extractor) Category() string {
	return "land"
}

// Extract normalizes one sanitized land listing into an item.Item. It
// returns an error only when the resulting item would fail item.Validate
// (e.g. an empty SourceKey) or the price is negative -- i.e. when no valid
// item of this category can be formed. A missing/unparseable acreage,
// keyword flag, location, or APN is NOT an error; those attributes are
// simply left unset. Unpriced or vague land listings are common, and the
// downstream hard-filter stage is responsible for enforcing and explaining
// any such requirement.
func (e *Extractor) Extract(_ context.Context, s listing.Sanitized) (item.Item, error) {
	it := item.Item{
		ID:         deterministicID(s.SourceID, s.SourceKey),
		Category:   "land",
		Class:      item.ClassDurable,
		Title:      s.Title, // untrusted-as-data: carried verbatim, never interpreted
		PriceCents: s.PriceCents,
		Currency:   s.Currency,
		SourceID:   s.SourceID,
		SourceKey:  s.SourceKey,
		SourceURL:  s.SourceURL,
		SeenAt:     s.SeenAt,
		Attributes: map[string]string{},
		Tokens:     tokenize(s.Title),
	}

	// Title and Body are both inert scan targets -- never interpreted, only
	// matched against fixed regex/dictionary patterns below.
	combined := s.Title + "\n" + s.Body

	it.CanonicalID = extractAPN(combined)

	if acreage, ok := extractAcreage(combined); ok {
		it.Attributes["acreage"] = acreage
	}

	for _, f := range flagPatterns {
		if f.re.MatchString(combined) {
			it.Attributes[f.attr] = "true"
		}
	}

	if loc, ok := s.Aspects["location"]; ok && strings.TrimSpace(loc) != "" {
		it.Attributes["location"] = loc
	}

	if err := it.Validate(); err != nil {
		return item.Item{}, fmt.Errorf("land: extract: %w", err)
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

// acreRe matches "N acre"/"N acres" (singular or plural), the most explicit
// and unambiguous acreage phrasing. acRe matches the common abbreviation "N
// ac" as a standalone word (so "ac" is not picked out of some longer word).
// Both are tried before falling back to a square-footage conversion, per the
// preference: an explicit acres match beats a sqft match.
var (
	acreRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*acres?\b`)
	acRe   = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*ac\b`)
	sqftRe = regexp.MustCompile(`(?i)(\d[\d,]*)\s*(?:sq\.?\s*ft|sqft|square\s+feet)\b`)
)

// extractAcreage scans text for an acreage figure, preferring an explicit
// "acre(s)"/"ac" phrasing and falling back to a "sq ft"/"square feet" figure
// converted to acres (divided by 43560, the number of square feet per acre,
// rounded to 2 decimal places). It reports ok=false (and sets no attribute)
// when nothing matches -- that is an absent fact, not an error.
func extractAcreage(text string) (string, bool) {
	if m := acreRe.FindStringSubmatch(text); m != nil {
		if v, ok := formatTrimmed(m[1]); ok {
			return v, true
		}
	}
	if m := acRe.FindStringSubmatch(text); m != nil {
		if v, ok := formatTrimmed(m[1]); ok {
			return v, true
		}
	}
	if m := sqftRe.FindStringSubmatch(text); m != nil {
		raw := strings.ReplaceAll(m[1], ",", "")
		sqft, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return "", false
		}
		acres := math.Round((sqft/43560)*100) / 100
		return strconv.FormatFloat(acres, 'f', -1, 64), true
	}
	return "", false
}

// formatTrimmed parses raw as a float and re-renders it in the shortest form
// that round-trips, which has the effect of trimming trailing zeros (e.g.
// "5.50" -> "5.5", "10.0" -> "10").
func formatTrimmed(raw string) (string, bool) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatFloat(v, 'f', -1, 64), true
}

// flagPattern pairs a boolean Attributes key with the regex whose presence
// (as a whole word / phrase) sets it to "true". Absence of a match leaves the
// attribute unset entirely (never a "false" value), consistent with the rest
// of the extractor's absence-is-not-an-error stance.
type flagPattern struct {
	attr string
	re   *regexp.Regexp
}

var flagPatterns = []flagPattern{
	// well: NOTE this is a simple \bwell\b whole-word match, so it will also
	// fire on phrasing like "well-maintained" or "as well" that mention a
	// water well only incidentally (or not at all). This is a known,
	// accepted source of noise in v1 -- the alternative (a denylist of every
	// "well ..." idiom) is itself unbounded and not worth the complexity for
	// a keyword-presence flag that the hard-filter stage treats as a soft
	// signal, not a guarantee.
	{"well", regexp.MustCompile(`(?i)\bwell\b`)},
	{"septic", regexp.MustCompile(`(?i)\bseptic\b`)},
	{"fixer", regexp.MustCompile(`(?i)\b(fixer|teardown|tear-down|handyman|as-is|as is)\b`)},
	{"manufactured", regexp.MustCompile(`(?i)\b(manufactured|mobile\s+home|modular)\b`)},
	{"owner_financing", regexp.MustCompile(`(?i)\b(owner|seller)\s+financ`)},
}

// apnRe best-effort matches an Assessor's Parcel Number introduced by the
// literal label "APN" (optionally followed by ":" or "#"), e.g.
// "APN: 123-456-789" or "APN#123-45-678". It requires at least 5 digit/dash
// characters after the label so it does not pick up a bare "APN 1" or
// similarly short, likely-spurious fragment.
var apnRe = regexp.MustCompile(`(?i)\bAPN[:#]?\s*([0-9][0-9\-]{4,})`)

// extractAPN best-effort scans text for an APN. It returns "" (not an error)
// when no APN-labeled figure is present.
func extractAPN(text string) string {
	m := apnRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
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
