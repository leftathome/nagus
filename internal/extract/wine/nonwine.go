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
// product_type or tags declare the listing non-wine, else nil. Culinary wins
// over merchandise when both are declared (a pantry item tagged merch).
func declaredNonWine(aspects map[string]string) error {
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
// Hill Syrah" and "JaM Cellars Butter Chardonnay" are wines whose producer
// or name contains a food word. The head cue is a varietal, an appellation,
// a fortified style or a colour keyword; a year is not one ("Apricot Jam
// 2021"). A conjunction between them ("Cheese and Port Gift Set") means the
// wine is not the head.
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

// headCueWords are single words that are wine cues for the head rule; the
// varietal and appellation tables supply the rest.
var headCueWords = setOfWords("port", "sherry", "madeira", "marsala", "angelica", "colheita", "lbv",
	"fino", "manzanilla", "amontillado", "oloroso", "sparkling", "champagne", "prosecco", "cava",
	"rose", "brut")

// culinaryTitle reports whether a title is a food made with wine.
func culinaryTitle(title string) bool {
	words := normalizeWords(title)
	last := -1
	for i, w := range words {
		if culinaryNouns[w] {
			last = i
		}
		if i+1 < len(words) && culinaryPairs[w+" "+words[i+1]] {
			last = i + 1
		}
	}
	if last < 0 {
		return false
	}
	// A spirit or beer is neither wine nor a food ("Revelton Honey
	// Whiskey", "Honey Cask Bourbon"): it falls through to not-wine.
	for _, w := range words {
		if spiritWords[w] && w != "malt" {
			return false
		}
	}
	for i := last + 1; i < len(words); i++ {
		if conjunctions[words[i]] {
			return true
		}
		if headCue(words, i) {
			return false
		}
	}
	return true
}

// headCue reports whether a wine cue starts at words[i].
func headCue(words []string, i int) bool {
	if headCueWords[words[i]] {
		return true
	}
	for _, v := range varietals {
		kw := strings.Fields(v.keyword)
		if i+len(kw) <= len(words) && strings.Join(words[i:i+len(kw)], " ") == v.keyword {
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
