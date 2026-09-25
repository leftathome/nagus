// Package wine implements the "wine" category listing.Extractor: it lifts a
// glovebox-sanitized listing.Sanitized into a normalized item.Item using
// deterministic regex/dictionary rules (no LLM in this slice).
//
// Like the other extractors this is the injection-containment stage (design
// section 7): the output is a CONSTRAINED TYPED SCHEMA. Listing text is only
// ever pattern-matched -- a malicious listing can at worst yield a wrong
// field value ("WS 99" it never earned), never hijack anything. The spec's
// LLM step (structuring odd free-text critic attributions the regexes miss)
// is a deliberate follow-on that would run on this same sanitized text and
// emit only typed labels.
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
//
// Wine IDENTITY is not extracted here. The LWIN resolver that used to stamp
// CanonicalID moved to quark (quark QUARK-04, design D1: identity belongs
// wholly to quark): a wine source's offers carry its declared producer and
// sanitized title to quark as a name hint (pipeline.Ingester.NameHintProducer),
// and quark's answer -- a product id, only ever from an auto-band match --
// lands on the offer, not the item. Wine items therefore carry no CanonicalID.
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
	"time"
	"unicode"

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
}

var _ listing.Extractor = (*Extractor)(nil)

// New returns an Extractor for the "wine" category.
func New() *Extractor {
	return &Extractor{}
}

// Category returns "wine".
func (e *Extractor) Category() string {
	return "wine"
}

// Extract normalizes one sanitized wine listing. Missing signal (no vintage,
// no critic scores) is absence, not an error -- the
// hard-filter and valuation stages own enforcing and explaining any
// requirements. An error is returned only when no valid item can be formed.
func (e *Extractor) Extract(_ context.Context, s listing.Sanitized) (item.Item, error) {
	if culinaryRe.MatchString(s.Title) {
		return item.Item{}, fmt.Errorf("wine: extract: %w", ErrCulinary)
	}
	if isMerchandise(s.Title) {
		return item.Item{}, fmt.Errorf("wine: extract: %w", ErrMerchandise)
	}
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

	// Precedence for vintage and varietal: what the TITLE says, then the
	// source's structured field, then the description. Commerce7 and
	// Vinoshipper publish vintage/varietal/type/bottle size as data, which
	// fills gaps ("Esprit de Tablas" names neither vintage nor grapes) and
	// beats a description that mentions another grape (Kiona's "Old Vine
	// Chenin Blanc"). But stores reuse product records across releases, so
	// the data can be stale: K Vintners' "2022 Broncho Malbec" carries
	// vintage 2021 and Analemma's Mencia carries Pinot Noir (2026-09-21).
	// The title is what the buyer sees, so it wins.
	vintage, hasVintage := extractVintage(s.Title)
	if !hasVintage {
		if v, err := strconv.Atoi(s.Aspects["vintage"]); err == nil && v > 1800 && v < 2200 {
			vintage, hasVintage = v, true
		} else {
			vintage, hasVintage = extractVintage(text)
		}
	}
	if hasVintage {
		it.Attributes["vintage"] = strconv.Itoa(vintage)
	}
	it.Attributes["bottle_ml"] = strconv.Itoa(extractBottleML(text))
	if ml, err := strconv.Atoi(s.Aspects["bottle_ml"]); err == nil && ml > 0 {
		it.Attributes["bottle_ml"] = strconv.Itoa(ml)
	}
	sourceVarietal := strings.TrimSpace(s.Aspects["varietal"])
	if varietal, colour, ok := extractVarietal(s.Title); ok {
		it.Attributes["varietal"] = varietal
		it.Attributes["colour"] = colour
	} else if sourceVarietal != "" {
		it.Attributes["varietal"] = sourceVarietal
		if _, colour, ok := extractVarietal(sourceVarietal); ok {
			it.Attributes["colour"] = colour
		}
	} else if varietal, colour, ok := extractVarietal(text); ok {
		it.Attributes["varietal"] = varietal
		it.Attributes["colour"] = colour
	}
	if it.Attributes["colour"] == "" {
		if colour, ok := extractColour(text); ok {
			it.Attributes["colour"] = colour
		}
	}
	// The source's wine type is the colour ("Rose of Pinot Noir" is a rose,
	// not the red its grape implies).
	if c := sourceColour(s.Aspects["wine_type"]); c != "" {
		it.Attributes["colour"] = c
	}

	// When the store published the product (Shopify published_at): a recent
	// date is a new-release signal a watch can ping on (new_within_days).
	if d, err := time.Parse("2006-01-02", s.Aspects["published_at"]); err == nil {
		it.Attributes["published_at"] = d.Format("2006-01-02")
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

	// Store sale: the store's own list price, when it says the listing is on
	// sale. A signal that needs no critic scores and no price history -- the
	// only deal signal the configured producer stores can offer today.
	if cmp, err := strconv.ParseInt(s.Aspects["compare_at_cents"], 10, 64); err == nil && cmp > s.PriceCents && s.PriceCents > 0 {
		it.Attributes["list_price_cents"] = strconv.FormatInt(cmp, 10)
		it.Attributes["discount_pct"] = strconv.FormatInt((cmp-s.PriceCents)*100/cmp, 10)
	}

	// The producer, when the source declared or published one: rows show
	// it, and joins that cannot use a product id (a label approval names a
	// brand, not a vintage) join on it.
	if p := strings.TrimSpace(s.Aspects["wine_producer"]); p != "" {
		it.Attributes["producer"] = p
	}

	// An explicit non-vintage marker ("NV", "N.V."): a house-style blend
	// whose listing states no year on purpose. It tells the comparison key
	// (offer.ComparisonKey) that a wine quark cannot classify is non-vintage,
	// and it is wine evidence -- but only beside another wine cue, because
	// "NV" is also a US state ("Pickup Fee Reno, NV"), a company suffix
	// ("Heineken N.V."), and a word merchandise titles carry ("Gift Box NV").
	if explicitNV(s.Title, it.Attributes["varietal"] != "" || it.Attributes["colour"] != "") {
		it.Attributes["nv"] = "true"
	}

	// A disgorgement is wine evidence on its own: only sparkling wine is
	// disgorged. Its year is never the vintage (extractVintage skips it).
	disgorged := disgorgedRe.MatchString(s.Title)

	// A fortified-wine style is wine evidence on its own, NV or not (operator
	// ruling 2026-09-25, "Port is wine"; LWIN files every one of them as
	// "Fortified Wine", which quark loads). See fortifiedWine for the guards.
	fortified, culinaryFortified := fortifiedWine(s.Title)

	// No wine evidence at all -- no vintage, no varietal, no colour, no NV
	// marker, no disgorgement, no fortified style -- means merchandise the
	// keyword list did not name. A source-declared wine_type is a colour
	// (sourceColour above), so it passes this check for any title the
	// merchandise lists did not already reject: intentional, and unchanged
	// since production (TestExtract_DeclaredWineTypeIsEvidence). Measured on the live corpus
	// (2026-09-21): exactly 12 of 91 wine items had none of the three, and all
	// 12 were merchandise (a foil cutter and key chains among them, on sale,
	// which the wine-sales watch would have pinged); none of the 79 bottles.
	if it.Attributes["vintage"] == "" && it.Attributes["varietal"] == "" && it.Attributes["colour"] == "" && it.Attributes["nv"] == "" && !disgorged && !fortified {
		if culinaryFortified {
			// "Sherry Trifle Mix", "Marsala Chicken Sauce": a food made
			// with the wine.
			return item.Item{}, fmt.Errorf("wine: extract: %w", ErrCulinary)
		}
		return item.Item{}, fmt.Errorf("wine: extract: %w", ErrNotWine)
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

// notVintageBeforeRe is a word that, directly before a year (or with one
// filler word between: "bottled in 2020"), makes the year something other
// than the vintage: "disgorged 2019", "bottled 2021", "Est. 1970", "founded
// 1885", "since 1885", "anniversary 1966". "<ordinal> anniversary" is NOT
// one: in "50th Anniversary 2019 Cabernet" the year is the vintage; see
// ordinalAnniversaryRe.
var notVintageBeforeRe = regexp.MustCompile(`(?i)\b(disgorged|disgorgement|degorge|degorgement|bottled|est\.?|established|founded|since|anniversary)[\s:.,(]*((in|on|at|from)[\s:.,(]+)?$`)

// ordinalAnniversaryRe is "50th anniversary" and the like, right before a
// year.
var ordinalAnniversaryRe = regexp.MustCompile(`(?i)\b\d+(st|nd|rd|th)\s+anniversary[\s:.,(]*$`)

// fortifiedRe is a fortified-wine style: port and its categories, sherry and
// its styles, madeira, marsala. Whole words only ("Newport", "Portland",
// "Portsmouth", "Passport" never match). "tawny" and "ruby" are NOT here on
// their own -- they are colours and gemstones ("Ruby Red Grapefruit",
// "Tawny Leather Tote") -- but "Tawny Port" / "Ruby Port" match on "port".
var fortifiedRe = regexp.MustCompile(`(?i)\b(port|colheita|lbv|late bottled vintage|sherry|fino|manzanilla|amontillado|oloroso|pedro ximenez|px|madeira|marsala)\b`)

// notWinePortRe is "port" the English word: a place ("Port Townsend", "Port
// Angeles" -- a short list, not a gazetteer) or a connector ("USB port").
// The wine extractor rarely sees either; the tech half is defensive.
var notWinePortRe = regexp.MustCompile(`(?i)\bport\s+(townsend|angeles|orchard|ludlow|hadlock|huron|arthur|jefferson|washington|charlotte|richey|chester|clinton|royal|elizabeth|moresby|louis|lincoln|hueneme|aransas|lavaca|isabel|st\.?\s+lucie)\b|\b(usb|usb-c|hdmi|ethernet|charging|charger|serial|audio|thunderbolt|lightning|network|power|display)\s+ports?\b`)

// spiritOrBeerRe is a spirit or a beer anywhere in a title: a fortified-wine
// word there names the cask it was finished in, or a distillery ("Sherry
// Cask Bourbon", "Port Dundas Grain Whisky", "Sherry Barrel Imperial
// Porter"). It only withdraws the fortified cue: a title with a vintage, a
// varietal or a colour is judged on those ("Porter Creek Vineyards Pinot
// Noir 2021").
var spiritOrBeerRe = regexp.MustCompile(`(?i)\b(whisky|whiskey|scotch|bourbon|rum|gin|vodka|tequila|mezcal|brandy|cognac|armagnac|single malt|stout|porter|ale|ipa|lager|beer|cider)\b`)

// CULINARY PRODUCTS are not wine and are NOT merchandise: nagus may watch
// groceries one day, and a grocery category would claim them. The wine
// extractor rejects them with ErrCulinary (still ErrNotWine), so logs and
// skips count them apart from merchandise. Two lists:
//
//   - culinaryRe always rejects: nothing called vinegar or cheese is a
//     bottle of wine, whatever colour or varietal the title names ("Camino
//     Red Wine Vinegar", live on broc-cellars). Checked against the live
//     corpus: it rejects no wine.
//   - culinaryWordRe is words that real wine names also use ("JaM Cellars",
//     "The Chocolate Block", "2021 Mix"): they only withdraw the fortified
//     cue ("Sherry Trifle Mix", "Marsala Chicken Sauce").
var culinaryRe = regexp.MustCompile(`(?i)\b(vinegar|cooking (wine|sherry)|cakes?|cheeses?|jelly|olive oil)\b`)

var culinaryWordRe = regexp.MustCompile(`(?i)\b(cooking|jam|sauces?|trifle|mix|fudge|chocolates?)\b`)

// caskWordRe is a cask word: within two tokens of a fortified-wine word it
// names a cask finish, not the wine ("Port Cask Finish", "Sherry Oak").
var caskWordRe = regexp.MustCompile(`(?i)^(cask|casks|barrel|barrels|oak|wood|finish|finished)$`)

// fortifiedWine reports whether a title names a fortified-wine style once
// the English-word uses of "port" are set aside, and no cask finish, spirit
// or beer says the word is about something else. culinary reports a
// fortified-wine word withdrawn because the title is a food made with it.
func fortifiedWine(title string) (fortified, culinary bool) {
	t := notWinePortRe.ReplaceAllString(foldASCII(title), " ")
	named := false
	for _, m := range fortifiedRe.FindAllStringIndex(t, -1) {
		start, end := m[0], m[1]
		// A hyphen-joined token is not the word: "Port-a-Potty".
		if (start > 0 && t[start-1] == '-') || (end < len(t) && t[end] == '-') {
			continue
		}
		if nearCaskWord(t[:start], t[end:]) {
			continue
		}
		named = true
		break
	}
	switch {
	case !named || spiritOrBeerRe.MatchString(t):
		return false, false
	case culinaryWordRe.MatchString(t):
		return false, true
	}
	return true, false
}

// nearCaskWord reports whether one of the two tokens before or after a match
// is a cask word.
func nearCaskWord(before, after string) bool {
	notWord := func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }
	b, a := strings.FieldsFunc(before, notWord), strings.FieldsFunc(after, notWord)
	if len(b) > 2 {
		b = b[len(b)-2:]
	}
	if len(a) > 2 {
		a = a[:2]
	}
	for _, w := range append(b, a...) {
		if caskWordRe.MatchString(w) {
			return true
		}
	}
	return false
}

// disgorgedRe marks a disgorgement date: sparkling wine only.
var disgorgedRe = regexp.MustCompile(`(?i)\b(disgorged|disgorgement|degorged?|degorgement)\b`)

// extractVintage returns the FIRST plausible vintage year in the text, or
// (0, false) for none / a non-vintage (NV) wine. A year right after a word
// that says it is something else -- a disgorgement or bottling date, a
// founding year, an anniversary -- is not the vintage and is skipped.
func extractVintage(text string) (int, bool) {
	folded := foldASCII(text)
	for _, loc := range vintageRe.FindAllStringIndex(folded, -1) {
		if before := folded[:loc[0]]; notVintageBeforeRe.MatchString(before) && !ordinalAnniversaryRe.MatchString(before) {
			continue
		}
		v, err := strconv.Atoi(folded[loc[0]:loc[1]])
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
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
	{"grenache blanc", "Grenache Blanc", "white"},
	{"pinot blanc", "Pinot Blanc", "white"},
	{"gruner veltliner", "Gruner Veltliner", "white"},
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
	{"semillon", "Semillon", "white"},
	{"roussanne", "Roussanne", "white"},
	{"marsanne", "Marsanne", "white"},
	{"vermentino", "Vermentino", "white"},
	{"picpoul", "Picpoul", "white"},
	{"mencia", "Mencia", "red"},
	{"gamay", "Gamay", "red"},
	{"barbera", "Barbera", "red"},
	{"carignan", "Carignan", "red"},
	{"cinsault", "Cinsault", "red"},
	{"counoise", "Counoise", "red"},
	{"tannat", "Tannat", "red"},
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
// same practical set quark's LWIN normalizer uses).
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

// criticBracketRe matches the bracketed shorthand retailers put in titles:
// "2013 Vega-Sicilia Unico [JS98][WA97][WS96]", "[V90]". Bracketed is its own
// rule because two of these codes are unsafe as bare words: "WA" is also
// Washington State and "V" is a letter, so they are only trusted inside the
// brackets. WA (Wine Advocate) canonicalizes to RP and V to VM, matching
// criticNames, so a listing carrying both spellings cannot double-count.
var criticBracketRe = regexp.MustCompile(`\[(WS|JS|RP|WA|WE|JD|VM|V|DEC)\s*(\d{2,3})\+?\]`)

// bracketCritic canonicalizes a bracketed code.
var bracketCritic = map[string]string{"WA": "RP", "V": "VM"}

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
	for _, m := range criticBracketRe.FindAllStringSubmatch(text, -1) {
		code := m[1]
		if c, ok := bracketCritic[code]; ok {
			code = c
		}
		if v, err := strconv.ParseFloat(m[2], 64); err == nil {
			out = append(out, valwine.RawScore{Critic: code, Score: v, Scale: 100})
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

// nvRe finds an "NV" / "N.V." token, capturing what precedes and follows it.
var nvRe = regexp.MustCompile(`(?i)(^|[^a-z0-9])n\.?v\.?([^a-z0-9]|$)`)

// nvNotWineAfterRe is what, directly after "N.V.", makes it a company suffix
// rather than a wine marker ("Heineken N.V. Beer").
var nvNotWineAfterRe = regexp.MustCompile(`(?i)^\s*(beer|brewery|brewing|lager|ale|company|co\b|corp|inc\b|ltd|llc|holdings?|group)`)

// nvWineCueRe are the words that make a title's NV a wine's NV: sparkling
// and house-style terms. A colour or a varietal also qualifies (passed in).
var nvWineCueRe = regexp.MustCompile(`(?i)\b(extra brut|brut|cuvee|champagne|cremant|cava|prosecco|sparkling|rose|blanc de blancs|blanc de noirs|port|tawny|ruby|moscato|pet[ -]?nat|petillant|vermouth|vermut)\b`)

// nevadaPlaceRe is a Nevada place directly before "NV" with no comma
// ("Reno NV Pickup"): the state, not a non-vintage marker. The main cities
// only -- a gazetteer is out of scope, and the comma form covers the rest.
var nevadaPlaceRe = regexp.MustCompile(`(?i)\b(reno|las vegas|vegas|henderson|sparks|carson city|north las vegas|elko|mesquite|boulder city|incline village|stateline)$`)

// explicitNV reports whether a title marks the wine non-vintage: an NV token
// that is not a state after a city (", NV"), not a company suffix, and sits
// beside a wine cue -- a sparkling/house-style word, or (otherWineCue) a
// colour or varietal the extractor already found.
func explicitNV(title string, otherWineCue bool) bool {
	folded := foldASCII(title)
	for _, m := range nvRe.FindAllStringSubmatchIndex(folded, -1) {
		start, end := m[0], m[1]
		before := strings.TrimRight(folded[:start+len(folded[m[2]:m[3]])], " ")
		if strings.HasSuffix(before, ",") || nevadaPlaceRe.MatchString(before) {
			continue // "Reno, NV" / "Reno NV": the state after a city
		}
		if nvNotWineAfterRe.MatchString(folded[end-len(folded[m[4]:m[5]]):]) {
			continue // "Heineken N.V. Beer": a company suffix
		}
		return otherWineCue || nvWineCueRe.MatchString(notWinePortRe.ReplaceAllString(folded, " "))
	}
	return false
}

// ErrNotWine rejects a listing that is not a bottle of wine. The ingest
// pipeline records it as an extract skip (the reason is the error text) and
// deletes any item stored under it. The two named reasons below wrap it; a
// bare ErrNotWine is a title with no wine evidence at all.
var ErrNotWine = fmt.Errorf("%w: not a wine", listing.ErrNotInCategory)

// ErrMerchandise is a listing on the merchandise lists: a tote, a glass.
var ErrMerchandise = fmt.Errorf("%w (merchandise)", ErrNotWine)

// ErrCulinary is a culinary product: not wine, and not merchandise either
// -- reserved for a possible future grocery category (see culinaryRe).
var ErrCulinary = fmt.Errorf("%w (culinary; reserved for a future grocery category)", ErrNotWine)

// merchandiseRe matches what winery storefronts sell besides wine. A keyword
// rule, deliberately, rather than "no vintage, no varietal": plenty of real
// wines carry neither (Harbinger's non-vintage "Bolero").
var merchandiseRe = regexp.MustCompile(`(?i)(\b(tote|totes|gift card|e-?gift|corkscrews?|openers?|decanters?|glass(es|ware)?|stemware|aerators?|t-?shirts?|shirts?|hats?|caps|hoodies?|aprons?|coasters?|candles?|membership|wine club|tasting fee|tickets?|reservations?|shipping (fee|charge|cost|insurance|upgrade)|pickup fee|key ?chains?|foil cutters?|stoppers?)\b|^\s*shipping\b)`)

// packagingMerchRe are words that name merchandise ON THEIR OWN ("Champagne
// Flute", "Prosecco Ice Bucket", "Gift Box NV") but also appear on real wine
// sold in packaging ("6PK Gift Box Wood MRW, 2021 Mix"). They mark a title
// merchandise only when nothing in it says it is wine; see isMerchandise.
var packagingMerchRe = regexp.MustCompile(`(?i)\b(gift box(es)?|flutes?|buckets?|sab(er|re)s?|charms?|chillers?|sleeves?|towels?|soaps?|display|tools?|pickup|sippers?|leather|carriers?)\b`)

// packCountRe is a pack count: "6PK", "6 pk", "12-pack", "3 Pack".
var packCountRe = regexp.MustCompile(`(?i)\b\d+\s*-?\s*(pk|pack)s?\b`)

// isMerchandise reports whether a wine-store title is merchandise (nagus-17k:
// robert-mondavi-winery's "Single Bottle Wine Tote" was ingested as a wine).
//
// Two lists. merchandiseRe always wins: nothing called olive oil or a key
// chain is a bottle of wine, whatever year it carries ("2025 Fox Hill Olive
// Oil"). packagingMerchRe wins only when the title carries no pack count, no
// vintage year and no varietal -- a gift box of wine is wine.
func isMerchandise(title string) bool {
	if merchandiseRe.MatchString(title) {
		return true
	}
	if !packagingMerchRe.MatchString(title) {
		return false
	}
	if packCountRe.MatchString(title) || vintageRe.MatchString(title) {
		return false
	}
	_, _, varietal := extractVarietal(title)
	return !varietal
}

// sourceColour maps a source's structured wine type (Commerce7 "Red",
// Vinoshipper "RED", "ROSE", ...) onto the extractor's colour vocabulary; ""
// when the type names no colour (sparkling, dessert).
func sourceColour(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "red":
		return "red"
	case "white":
		return "white"
	case "rose", "ros\u00e9", "rosado", "rosato":
		return "rose"
	case "orange":
		return "orange"
	}
	return ""
}
