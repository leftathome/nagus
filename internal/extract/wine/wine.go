// Package wine implements the "wine" category listing.Extractor: it lifts a
// glovebox-sanitized listing.Sanitized into a normalized item.Item using
// deterministic regex/dictionary rules (no LLM in this slice).
//
// Like the other extractors this is the injection-containment stage (design
// section 7): the output is a CONSTRAINED TYPED SCHEMA. Listing text is only
// ever pattern-matched -- a malicious listing can at worst yield a wrong
// field value ("WS 99" it never earned), never hijack anything. The spec's
// LLM step (structuring odd free-text critic attributions the regexes miss,
// and adjudicating mid-confidence LWIN matches) is a deliberate follow-on
// that would run on this same sanitized text and emit only typed labels.
//
// Extracted signal:
//
//   - vintage / bottle_ml / colour / varietal -- regex + dictionary.
//   - critic scores -- retailer shorthand ("WS 92", "JS 94") and full names
//     ("Wine Spectator 92") are parsed into typed RawScores, then
//     normalized+aggregated by internal/valuation/wine into wine_score /
//     wine_score_count. NOTE: the "WA" shorthand for The Wine Advocate is
//     deliberately NOT recognized -- in this project's home market "WA" is
//     Washington state and appears next to numbers (zip codes, "WA 98362")
//     far too often; The Wine Advocate is matched as "RP" or by full name.
//   - wine_channel / source_origin / ship_legal_to -- lifted from connector
//     aspects, where the per-source channel tagger stamped them (legality is
//     a property of the SOURCE's shipping channel and origin jurisdiction
//     under the internal/shipping rules table, not derivable from listing
//     text). ship_legal_to is the token SET of destination JURISDICTIONS the
//     source may legally ship to ("US-WA", "CA-BC", "FR"); each token is
//     validated as an ISO 3166 code so an untrusted aspect can never smuggle
//     a non-jurisdiction token past the destination filter.
//   - CanonicalID -- when an LWIN resolver is injected, a HIGH-CONFIDENCE
//     (RouteAuto) match stamps the LWIN-11. Lower-confidence routes leave
//     CanonicalID empty and record lwin_route so the adjudication tier can
//     find them; a wrong canonical identity corrupts every downstream
//     quality join, so only auto-route matches are ever stamped.
package wine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/leftathome/nagus/internal/identity/lwin"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/shipping"
	valwine "github.com/leftathome/nagus/internal/valuation/wine"
)

// DefaultBottleML is assumed when a listing names no size: 750ml is the
// standard bottle and non-standard sizes are (by strong industry convention)
// always called out in the title ("1.5L", "Magnum", "375ml").
const DefaultBottleML = 750

// Extractor implements listing.Extractor for category "wine".
type Extractor struct {
	// Normalizer aggregates parsed critic scores; the zero value applies the
	// default anchors with no per-critic bias.
	Normalizer valwine.Normalizer
	// Resolver, when non-nil, resolves listings to LWIN identities. Only
	// RouteAuto matches stamp CanonicalID (see package doc).
	Resolver *lwin.Resolver
}

var _ listing.Extractor = (*Extractor)(nil)

// New returns an Extractor for the "wine" category with no LWIN resolver.
func New() *Extractor {
	return &Extractor{}
}

// Category returns "wine".
func (e *Extractor) Category() string {
	return "wine"
}

// Extract normalizes one sanitized wine listing. Missing signal (no vintage,
// no critic scores, no LWIN match) is absence, not an error -- the
// hard-filter and valuation stages own enforcing and explaining any
// requirements. An error is returned only when no valid item can be formed.
func (e *Extractor) Extract(_ context.Context, s listing.Sanitized) (item.Item, error) {
	text := s.Title
	if s.Body != "" {
		text += "\n" + s.Body
	}

	it := item.Item{
		ID:         deterministicID(s.SourceID, s.SourceKey),
		Category:   "wine",
		Class:      item.ClassDurable,
		Title:      s.Title, // untrusted-as-data: carried verbatim, never interpreted
		PriceCents: s.PriceCents,
		Currency:   s.Currency,
		Condition:  "new", // retail wine is always new stock; provenance/cellar sources would refine this
		SourceID:   s.SourceID,
		SourceKey:  s.SourceKey,
		SourceURL:  s.SourceURL,
		SeenAt:     s.SeenAt,
		Attributes: map[string]string{},
		Tokens:     tokenize(s.Title),
	}

	vintage, hasVintage := extractVintage(text)
	if hasVintage {
		it.Attributes["vintage"] = strconv.Itoa(vintage)
	}
	it.Attributes["bottle_ml"] = strconv.Itoa(extractBottleML(text))
	if varietal, colour, ok := extractVarietal(text); ok {
		it.Attributes["varietal"] = varietal
		it.Attributes["colour"] = colour
	} else if colour, ok := extractColour(text); ok {
		it.Attributes["colour"] = colour
	}

	// Critic attributions -> aggregated normalized quality score.
	raw := parseCriticScores(text)
	if mean, count := e.Normalizer.Aggregate(raw); count > 0 {
		it.Attributes["wine_score"] = strconv.FormatFloat(mean, 'f', 1, 64)
		it.Attributes["wine_score_count"] = strconv.Itoa(count)
		it.Attributes["critic_scores"] = renderRawScores(raw)
	}

	// Per-source channel/legality stamps (aspect values are untrusted like
	// any other: only the recognized vocabulary passes through). The
	// legal-destination set is re-rendered from its VALIDATED tokens, and is
	// stamped even when empty -- an empty set is a real, fail-closed fact
	// ("ships nowhere legally"), distinct from an untagged source.
	if ch := strings.TrimSpace(s.Aspects["wine_channel"]); ch != "" {
		it.Attributes["wine_channel"] = ch
	}
	if j, ok := shipping.NormJurisdiction(s.Aspects["source_origin"]); ok {
		it.Attributes["source_origin"] = j
	}
	if raw, present := s.Aspects["ship_legal_to"]; present {
		it.Attributes["ship_legal_to"] = strings.Join(jurisdictionTokens(raw), " ")
	}

	// LWIN identity resolution (optional).
	if e.Resolver != nil {
		res := e.Resolver.Resolve(lwin.Query{Name: s.Title, Vintage: vintage})
		it.Attributes["lwin_route"] = string(res.Route)
		if res.Route == lwin.RouteAuto {
			it.CanonicalID = res.Best.Record.LWIN11(vintage)
		}
	}

	if err := it.Validate(); err != nil {
		return item.Item{}, fmt.Errorf("wine: extract: %w", err)
	}
	return it, nil
}

// jurisdictionTokens validates and normalizes a space-separated set of
// jurisdiction codes, dropping anything that is not well-formed ISO 3166 (an
// untrusted aspect must not inject arbitrary tokens into the destination
// filter's search space).
func jurisdictionTokens(raw string) []string {
	var out []string
	for _, tok := range strings.Fields(raw) {
		if j, ok := shipping.NormJurisdiction(tok); ok {
			out = append(out, j)
		}
	}
	return out
}

// deterministicID derives a stable nagus id from source identity + key, with
// the same construction the other extractors use (truncated hex sha256 over
// "<sourceID>\x00<sourceKey>").
func deterministicID(sourceID, sourceKey string) string {
	sum := sha256.Sum256([]byte(sourceID + "\x00" + sourceKey))
	return hex.EncodeToString(sum[:])[:16]
}

// --- vintage ---

// vintageRe matches a plausible bottled-wine vintage year 1930-2049. The
// digit word boundaries keep it from firing inside "1500ml" or "20150".
var vintageRe = regexp.MustCompile(`\b(19[3-9]\d|20[0-4]\d)\b`)

// extractVintage returns the FIRST plausible vintage year in the text, or
// (0, false) for none / a non-vintage (NV) wine. 0 is also what the LWIN
// resolver treats as the NV vintage segment.
func extractVintage(text string) (int, bool) {
	m := vintageRe.FindString(text)
	if m == "" {
		return 0, false
	}
	v, err := strconv.Atoi(m)
	if err != nil {
		return 0, false
	}
	return v, true
}

// --- bottle size ---

var (
	litreRe = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*(?:l|liter|litre)s?\b`)
	mlRe    = regexp.MustCompile(`(?i)\b(\d{3,4})\s*ml\b`)
)

// sizeKeywords maps named formats to ml. Longer names are checked before
// their substrings ("double magnum" before "magnum").
var sizeKeywords = []struct {
	keyword string
	ml      int
}{
	{"double magnum", 3000},
	{"jeroboam", 3000},
	{"magnum", 1500},
	{"half bottle", 375},
	{"half-bottle", 375},
	{"demi", 375},
	{"split", 187},
}

// extractBottleML returns the bottle size in ml, defaulting to DefaultBottleML
// when the text names none (see the constant's doc for why a default is safe
// here when it would not be for, say, capacity).
func extractBottleML(text string) int {
	if m := mlRe.FindStringSubmatch(text); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil && v > 0 {
			return v
		}
	}
	if m := litreRe.FindStringSubmatch(text); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > 0 && v <= 30 {
			return int(v * 1000)
		}
	}
	lower := strings.ToLower(text)
	for _, sk := range sizeKeywords {
		if strings.Contains(lower, sk.keyword) {
			return sk.ml
		}
	}
	return DefaultBottleML
}

// --- varietal / colour ---

// varietals maps a lowercase varietal keyword to its canonical name and
// colour. Longer names first so "cabernet sauvignon" wins over any shorter
// overlap; the scan below respects declaration order.
var varietals = []struct {
	keyword  string
	varietal string
	colour   string
}{
	{"cabernet sauvignon", "Cabernet Sauvignon", "red"},
	{"sauvignon blanc", "Sauvignon Blanc", "white"},
	{"cabernet franc", "Cabernet Franc", "red"},
	{"pinot noir", "Pinot Noir", "red"},
	{"pinot gris", "Pinot Gris", "white"},
	{"pinot grigio", "Pinot Gris", "white"},
	{"chenin blanc", "Chenin Blanc", "white"},
	{"gewurztraminer", "Gewurztraminer", "white"},
	{"chardonnay", "Chardonnay", "white"},
	{"riesling", "Riesling", "white"},
	{"viognier", "Viognier", "white"},
	{"albarino", "Albarino", "white"},
	{"merlot", "Merlot", "red"},
	{"syrah", "Syrah", "red"},
	{"shiraz", "Syrah", "red"},
	{"zinfandel", "Zinfandel", "red"},
	{"malbec", "Malbec", "red"},
	{"grenache", "Grenache", "red"},
	{"sangiovese", "Sangiovese", "red"},
	{"nebbiolo", "Nebbiolo", "red"},
	{"tempranillo", "Tempranillo", "red"},
	{"mourvedre", "Mourvedre", "red"},
	{"petite sirah", "Petite Sirah", "red"},
	{"petit verdot", "Petit Verdot", "red"},
}

// extractVarietal scans for a known varietal keyword; the accent fold keeps
// "Gewürztraminer"/"Albariño" matching their ASCII dictionary entries.
func extractVarietal(text string) (varietal, colour string, ok bool) {
	lower := foldASCII(strings.ToLower(text))
	for _, v := range varietals {
		if strings.Contains(lower, v.keyword) {
			return v.varietal, v.colour, true
		}
	}
	return "", "", false
}

// colourKeywords is the fallback when no varietal names the colour.
// "white zinfandel" would be caught as red by the varietal table first --
// acceptable v1 noise, documented rather than special-cased.
var colourKeywords = []struct {
	keyword string
	colour  string
}{
	{"sparkling", "sparkling"},
	{"champagne", "sparkling"},
	{"prosecco", "sparkling"},
	{"cava", "sparkling"},
	{"rose", "rose"},
	{"red blend", "red"},
	{"red wine", "red"},
	{"white blend", "white"},
	{"white wine", "white"},
}

func extractColour(text string) (string, bool) {
	lower := foldASCII(strings.ToLower(text))
	for _, ck := range colourKeywords {
		if strings.Contains(lower, ck.keyword) {
			return ck.colour, true
		}
	}
	return "", false
}

// foldASCII folds the accented characters common in wine text to ASCII (the
// same practical set the LWIN normalizer uses).
var foldASCII = strings.NewReplacer(
	"à", "a", "â", "a", "ä", "a", "á", "a", "ã", "a",
	"ç", "c",
	"è", "e", "é", "e", "ê", "e", "ë", "e",
	"î", "i", "ï", "i", "í", "i",
	"ñ", "n",
	"ô", "o", "ö", "o", "ó", "o", "ø", "o",
	"û", "u", "ü", "u", "ú", "u",
).Replace

// --- critic scores ---

// criticCodeRe matches retailer shorthand: an UPPERCASE critic code followed
// by a 2-3 digit score, optionally separated by ":" or "-" and optionally
// suffixed "pts"/"points"/"+". Case-sensitive on the code by design: "ws 92"
// in prose is far more likely to be noise than an attribution.
var criticCodeRe = regexp.MustCompile(`\b(WS|JS|RP|WE|JD|VM|DEC)\s*[:\-]?\s*(\d{2,3})\+?\b`)

// criticJRRe matches Jancis Robinson's 20-point shorthand, allowing halves
// ("JR 17.5").
var criticJRRe = regexp.MustCompile(`\b(JR)\s*[:\-]?\s*(\d{1,2}(?:\.\d)?)\b`)

// criticNames maps full critic names (matched case-insensitively) to the
// canonical code and scale. "The Wine Advocate" and "Robert Parker" both
// canonicalize to RP so a listing carrying both cannot double-count.
var criticNames = []struct {
	name   string
	critic string
	scale  int
}{
	{"wine spectator", "WS", 100},
	{"james suckling", "JS", 100},
	{"wine enthusiast", "WE", 100},
	{"wine advocate", "RP", 100},
	{"robert parker", "RP", 100},
	{"jeb dunnuck", "JD", 100},
	{"vinous", "VM", 100},
	{"decanter", "DEC", 100},
	{"jancis robinson", "JR", 20},
}

// criticNameRes is built once from criticNames: "<name> ... <score>" with the
// same optional separators as the shorthand form.
var criticNameRes = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(criticNames))
	for i, cn := range criticNames {
		out[i] = regexp.MustCompile(`(?i)\b` + strings.ReplaceAll(cn.name, " ", `\s+`) + `\s*[:\-]?\s*(\d{1,3}(?:\.\d)?)\+?\b`)
	}
	return out
}()

// parseCriticScores extracts every recognizable critic attribution from the
// text as typed RawScores. Range validation belongs to the Normalizer
// (out-of-range parses are dropped there), duplicate critics are deduped by
// Aggregate -- this function only recognizes and types.
func parseCriticScores(text string) []valwine.RawScore {
	var out []valwine.RawScore
	for _, m := range criticCodeRe.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.ParseFloat(m[2], 64); err == nil {
			out = append(out, valwine.RawScore{Critic: m[1], Score: v, Scale: 100})
		}
	}
	for _, m := range criticJRRe.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.ParseFloat(m[2], 64); err == nil {
			out = append(out, valwine.RawScore{Critic: "JR", Score: v, Scale: 20})
		}
	}
	for i, re := range criticNameRes {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				out = append(out, valwine.RawScore{Critic: criticNames[i].critic, Score: v, Scale: criticNames[i].scale})
			}
		}
	}
	return out
}

// renderRawScores renders parsed attributions compactly ("JS:94 WS:92"),
// deduped per critic (highest wins, mirroring Aggregate) and sorted for
// determinism. Display data only -- never re-parsed.
func renderRawScores(raw []valwine.RawScore) string {
	best := map[string]valwine.RawScore{}
	for _, r := range raw {
		key := strings.ToUpper(r.Critic)
		if prev, ok := best[key]; !ok || r.Score > prev.Score {
			best[key] = r
		}
	}
	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+":"+strconv.FormatFloat(best[k].Score, 'f', -1, 64))
	}
	return strings.Join(parts, " ")
}

// tokenRe splits a title into raw token candidates on anything that is not an
// ASCII letter or digit.
var tokenRe = regexp.MustCompile(`[^a-z0-9]+`)

// tokenize lowercases title, splits on non-alphanumeric runs, drops empty and
// single-character tokens, and dedupes preserving first-seen order (same
// convention as the other extractors).
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
