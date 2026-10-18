package promo

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/leftathome/nagus/internal/deal"
)

// Drop reasons at the agreement guard.
const (
	DropNoProduct     = "no_product"
	DropMultiProduct  = "multi_product"
	DropBadPrice      = "bad_price"
	DropPriceDisagree = "price_disagree"
	DropTitleDisagree = "title_disagree"
	DropCurrency      = "currency"
	DropPriceSanity   = "price_sanity"
	DropNoTitle       = "no_title"
	// DropLinkTitleMismatch: the product URL the tracked link led to names
	// another product than the image (an open-time image and its click are
	// rendered separately and can disagree).
	DropLinkTitleMismatch = "link_title_mismatch"
	// DropLinkUnverifiable: the resolved URL has no name slug, and the
	// profile does not accept such links.
	DropLinkUnverifiable = "link_unverifiable"
)

// MinTitleAgreement is the least token overlap (Jaccard) two reads' titles
// must have.
const MinTitleAgreement = 0.5

// Product is what both reads agreed on.
type Product struct {
	Title    string
	Price    string // decimal major units, as nagus.deal/v1 accepts it
	WasPrice string // "" when not agreed
	Brand    string // "" when not agreed
}

// normPrice turns a read price into nagus.deal/v1 form: a leading currency
// mark ("$", "US$", "USD"), spaces and thousands commas are removed, then the
// deal price rule applies. ok is false for anything else.
func normPrice(v any) (string, bool) {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case json.Number:
		s = t.String()
	default:
		return "", false
	}
	s = strings.TrimSpace(s)
	for _, p := range []string{"US$", "USD", "$"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, p))
	}
	// Thousands commas only in their proper places ("1,299.99"); "1,2,9"
	// or "12,99" (a decimal comma) is not a price nagus can trust.
	if !priceShape.MatchString(s) {
		return "", false
	}
	s = strings.ReplaceAll(s, ",", "")
	if _, ok := deal.Price(s).Cents(); !ok {
		return "", false
	}
	// "589.9" and "589.90" are the same price: compare as cents.
	return s, true
}

var (
	priceShape = regexp.MustCompile(`^(?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.[0-9]+)?$`)
	numberRe   = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?`)
)

// MaxWasRatio caps a was-price at this multiple of the price: beyond it the
// was-price is not believed (the product is kept, the discount is not).
const MaxWasRatio = 5

// numbers is the set of numbers in a title ("12TB" and "12 TB" both give
// "12"; "2022" a vintage; "13.4in" gives "13.4").
func numbers(s string) map[string]bool {
	out := map[string]bool{}
	for _, n := range numberRe.FindAllString(s, -1) {
		out[strings.TrimLeft(n, "0")] = true
	}
	return out
}

// numbersConfirmed reports whether every number in title a (the one that
// is stored) is also in title b. The words may differ and b may add a
// number a lacks ("7200 RPM", "750ml"); but a capacity, vintage or model
// number in the stored title must be confirmed by the other read:
// "IronWolf 8TB" and "IronWolf 18TB" are different products.
func numbersConfirmed(a, b string) bool {
	nb := numbers(b)
	for n := range numbers(a) {
		if !nb[n] {
			return false
		}
	}
	return true
}

// clipTitle bounds a title to deal.MaxTitleLen characters.
func clipTitle(s string) string {
	r := []rune(s)
	if len(r) <= deal.MaxTitleLen {
		return s
	}
	return strings.TrimSpace(string(r[:deal.MaxTitleLen]))
}

func cents(s string) int64 {
	c, _ := deal.Price(s).Cents()
	return c
}

// foldAccents removes diacritics (NFD, combining marks dropped), so
// "Chateau" matches "Ch\u00e2teau" and "Cotes du Rhone" "C\u00f4tes du Rh\u00f4ne".
func foldAccents(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tokens is the lower-cased, accent-folded alphanumeric token set of a text.
func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(strings.ToLower(foldAccents(s)), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		out[f] = true
	}
	return out
}

// titleAgreement is the Jaccard overlap of two titles' token sets.
func titleAgreement(a, b string) float64 {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

func currencyOK(c *string) bool {
	if c == nil {
		return true
	}
	v := strings.ToUpper(strings.TrimSpace(*c))
	return v == "" || v == "USD" || v == "$" || v == "US$"
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// Agree applies the hallucination guard to two independent reads of one
// image. It returns the agreed product, or the reason there is none.
func Agree(a, b Reading) (Product, string) {
	switch {
	case len(a.Products) == 0 || len(b.Products) == 0:
		if len(a.Products) > 1 || len(b.Products) > 1 {
			return Product{}, DropMultiProduct
		}
		return Product{}, DropNoProduct
	case len(a.Products) > 1 || len(b.Products) > 1:
		return Product{}, DropMultiProduct
	}
	pa, pb := a.Products[0], b.Products[0]
	title := clipTitle(strings.Join(strings.Fields(pa.Title), " "))
	if title == "" || strings.TrimSpace(pb.Title) == "" {
		return Product{}, DropNoTitle
	}
	if !currencyOK(pa.Currency) || !currencyOK(pb.Currency) {
		return Product{}, DropCurrency
	}
	priceA, okA := normPrice(pa.Price)
	priceB, okB := normPrice(pb.Price)
	if !okA || !okB {
		return Product{}, DropBadPrice
	}
	if cents(priceA) != cents(priceB) {
		return Product{}, DropPriceDisagree
	}
	if titleAgreement(pa.Title, pb.Title) < MinTitleAgreement || !numbersConfirmed(title, pb.Title) {
		return Product{}, DropTitleDisagree
	}
	out := Product{Title: title, Price: priceA}
	wasA, wokA := normPrice(pa.WasPrice)
	wasB, wokB := normPrice(pb.WasPrice)
	if wokA && wokB && cents(wasA) == cents(wasB) {
		if cents(wasA) <= cents(priceA) {
			// A "was" price at or below the price is a misread of one of
			// them: the product is not trusted at all.
			return Product{}, DropPriceSanity
		}
		if cents(wasA) <= MaxWasRatio*cents(priceA) {
			out.WasPrice = wasA
		}
	}
	if ba, bb := str(pa.Brand), str(pb.Brand); ba != "" && strings.EqualFold(ba, bb) {
		out.Brand = strings.Join(strings.Fields(ba), " ")
	}
	return out, ""
}
