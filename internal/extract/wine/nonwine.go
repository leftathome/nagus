package wine

import (
	"strings"
)

// STORE-DECLARED NON-WINE. A store that files a listing under a non-wine
// product type or tag has told us what it is, and that beats any evidence
// the title or body could supply: Broc Cellars' "June Taylor Mission Fig +
// Angelica Jam" (product_type "Pantry", tags merch and pantry) was stored as
// a 2020 wine because its description names "our 2020 Angelica dessert
// wine" (production, 2026-09-25). The declaration only ever REJECTS: a store
// type or tag never makes a listing wine, and only the explicit non-wine
// words below count, so a real wine tagged "gifts" or "holiday" stays wine.
//
// Sources: Shopify's product_type and tags aspects (the shopify connector
// stamps both). Commerce7 and Vinoshipper already skip non-wine product
// types in the connector; OrderPort publishes no type.

// declaredCulinary / declaredMerchandise are normalized product types.
var (
	declaredCulinaryTypes = setOfWords("pantry", "food", "foods", "grocery", "groceries")
	declaredMerchTypes    = setOfWords("merch", "merchandise", "apparel", "gift card", "gift cards",
		"accessories", "wine accessories", "glassware", "book", "books", "event", "events",
		"special event", "special events", "ticket", "tickets", "membership", "memberships")
	declaredCulinaryTags = setOfWords("pantry", "food")
	declaredMerchTags    = setOfWords("merch", "merchandise", "apparel", "gift card")
)

// declaredNonWine returns ErrCulinary or ErrMerchandise when the source's
// product_type or tags declare the listing non-wine, else nil. When a store
// declares BOTH (Broc files art prints and jams alike under "Pantry" with a
// merch tag), the title decides: a food title is culinary ("June Taylor
// Mission Fig + Angelica Jam"), anything else merchandise ("Marta's Prints").
func declaredNonWine(aspects map[string]string, title string) error {
	culinary, merch := false, false
	pt := strings.Join(strings.Fields(strings.ToLower(aspects["product_type"])), " ")
	culinary = declaredCulinaryTypes[pt]
	merch = declaredMerchTypes[pt]
	for _, tag := range strings.Split(aspects["tags"], ",") {
		tag = strings.Join(strings.Fields(strings.ToLower(tag)), " ")
		culinary = culinary || declaredCulinaryTags[tag]
		merch = merch || declaredMerchTags[tag]
	}
	switch {
	case culinary && merch:
		if culinaryTitle(title) == ErrCulinary {
			return ErrCulinary
		}
		return ErrMerchandise
	case culinary:
		return ErrCulinary
	case merch:
		return ErrMerchandise
	}
	return nil
}

// CULINARY TITLES. A title naming a food -- a vinegar, a jam, a cheese -- is
// culinary UNLESS a wine cue follows the food word as the head of the title.
// English puts the head noun last: "Sherry Vinegar", "Madeira Cake",
// "Chardonnay Jam" and "Camino Red Wine Vinegar" are foods made with wine,
// while "Cake Bread Cellars Chardonnay", "Jelly Roll Zinfandel", "Vinegar
// Hill Syrah", "Honey Badger Red Blend" and "JaM Cellars Butter Chardonnay"
// are wines whose producer or name contains a food word. The head cue is a
// varietal, an appellation, a fortified style or a colour word or keyword; a
// year is not one ("Apricot Jam 2021"). A conjunction between them ("Honey
// and Port") means the wine is not the head. A food word that is plainly
// part of a producer name -- after "by", or before Cellars, Vineyards,
// Winery, Wines, Estate or Family ("Butter Chardonnay by JaM", "JaM Cellars
// Butter") -- is not a food at all.
//
// A food title that is also a bundle ("Spritz Pack w/ June Taylor Seasonal
// Fruit Syrup": a bottle and a syrup) is merchandise, not culinary, so a
// future grocery category does not claim it. Mead ("Honey Wine", "Honey
// Mead") is neither grape wine nor a food: plain not-wine.
//
// This runs before any other evidence, so a vintage from the description or
// the store's tags cannot make "June Taylor Mission Fig + Angelica Jam" a
// wine.

// culinaryNouns are single-word food nouns; culinaryPairs are two-word ones.
var (
	culinaryNouns = setOfWords("vinegar", "vinegars", "cake", "cakes", "cheese", "cheeses", "jelly",
		"jellies", "jam", "jams", "preserve", "preserves", "marmalade", "marmalades", "chutney",
		"chutneys", "compote", "compotes", "syrup", "syrups", "honey", "mustard", "mustards")
	culinaryPairs = setOfWords("olive oil", "cooking wine", "cooking sherry")
)

// conjunctions separate two products in one title.
var conjunctions = setOfWords("and", "with", "plus", "pairing", "paired", "for")

// bundleWords mark a title as several products sold together.
var bundleWords = setOfWords("pack", "packs", "bundle", "bundles", "set", "sets", "kit", "kits", "duo", "trio", "w", "with")

// producerAfter are words that, right after a food noun, make it part of a
// producer's name ("JaM Cellars").
var producerAfter = setOfWords("cellars", "cellar", "vineyard", "vineyards", "winery", "wines", "estate", "family")

// meadWords: honey wine is mead, not grape wine and not a food.
var meadWords = setOfWords("mead", "meads", "hydromel", "metheglin", "melomel")

// headCueWords are single words that are wine cues for the head rule; the
// varietal, appellation and colour tables (bareColours, colourKeywords)
// supply the rest.
var headCueWords = setOfWords("port", "sherry", "madeira", "marsala", "angelica", "colheita", "lbv",
	"fino", "manzanilla", "amontillado", "oloroso", "brut")

// nounHead finds the LAST noun in words that nounAt recognizes (nounAt
// returns the noun's length in words, 0 for none), skipping nouns that are
// part of a producer name, and reports whether that noun is the title's head:
// no wine cue follows it before a conjunction. found is false when no noun
// counts.
func nounHead(words []string, nounAt func(words []string, i int) int) (found, head bool) {
	last := -1
	for i := range words {
		n := nounAt(words, i)
		if n == 0 {
			continue
		}
		if i > 0 && words[i-1] == "by" {
			continue // "Butter Chardonnay by JaM"
		}
		if i+n < len(words) && producerAfter[words[i+n]] {
			continue // "JaM Cellars"
		}
		last = i + n - 1
	}
	if last < 0 {
		return false, false
	}
	for i := last + 1; i < len(words); i++ {
		if conjunctions[words[i]] {
			return true, true
		}
		if headCue(words, i) {
			return true, false
		}
	}
	return true, true
}

// culinaryNounAt is nounAt for food nouns.
func culinaryNounAt(words []string, i int) int {
	if i+1 < len(words) && culinaryPairs[words[i]+" "+words[i+1]] {
		return 2
	}
	if culinaryNouns[words[i]] {
		return 1
	}
	return 0
}

// culinaryTitle returns ErrCulinary for a food made with wine,
// ErrMerchandise for a food sold in a bundle, and nil otherwise.
func culinaryTitle(title string) error {
	words := normalizeWords(title)
	found, head := nounHead(words, culinaryNounAt)
	if !found || !head {
		return nil
	}
	for i, w := range words {
		// A spirit or beer is neither wine nor a food ("Revelton Honey
		// Whiskey", "Honey Cask Bourbon"): it falls through to not-wine.
		if spiritWords[w] && w != "malt" {
			return nil
		}
		if meadWords[w] || (w == "honey" && i+1 < len(words) && words[i+1] == "wine") {
			return nil
		}
	}
	for _, w := range words {
		if bundleWords[w] {
			return ErrMerchandise
		}
	}
	return ErrCulinary
}

// headCue reports whether a wine cue starts at words[i].
func headCue(words []string, i int) bool {
	if headCueWords[words[i]] {
		return true
	}
	if _, ok := bareColours[words[i]]; ok {
		return true // "Mustard Seed Red 2020", "Honey Bee White Wine"
	}
	for _, ck := range colourKeywords {
		if phraseAt(words, i, ck.keyword) {
			return true // "Jam Session Red Blend"
		}
	}
	for _, v := range varietals {
		if phraseAt(words, i, v.keyword) {
			return true
		}
	}
	for n := min(appellationMaxWords, len(words)-i); n >= 1; n-- {
		if known, broad := appellationTier(strings.Join(words[i:i+n], " ")); known && !broad {
			return true
		}
	}
	return false
}

// phraseAt reports whether the space-separated phrase starts at words[i].
func phraseAt(words []string, i int, phrase string) bool {
	kw := strings.Fields(phrase)
	return i+len(kw) <= len(words) && strings.Join(words[i:i+len(kw)], " ") == phrase
}

// objectNouns are objects no bottle of wine is sold as. See isMerchandise.
var objectNouns = setOfWords("towel", "towels", "charm", "charms", "soap", "soaps", "flute", "flutes", "tool", "tools")

// objectHead reports an object noun that is the title's head ("Merlot Tea
// Towel"), not a name word before the wine ("Charm City Syrah 2020", "Tool
// Shed Red 2019").
func objectHead(title string) bool {
	found, head := nounHead(normalizeWords(title), func(words []string, i int) int {
		if objectNouns[words[i]] {
			return 1
		}
		return 0
	})
	return found && head
}
