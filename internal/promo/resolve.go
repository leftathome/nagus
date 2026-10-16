package promo

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MaxHops bounds one tracked-link resolution.
const MaxHops = 5

// Drop reasons at link resolution.
const (
	DropLinkOffsite     = "link_offsite"
	DropHTTPDowngrade   = "http_downgrade"
	DropRedirectLoop    = "redirect_loop"
	DropTooManyHops     = "too_many_redirects"
	DropLinkUnresolved  = "link_unresolved"
	DropLinkFailed      = "link_failed" // transport error or 5xx: retried later
	DropBadItemNumber   = "bad_item_number"
	DropNotProductPath  = "not_a_product_url"
	DropMissingLinkHref = "no_product_link"
)

var (
	neweggNumeric = regexp.MustCompile(`^([0-9]{2})-([0-9]{3})-([0-9]{3})$`)
	neweggMarket  = regexp.MustCompile(`^9SI[0-9A-Z]{8,14}$`)
	neweggParent  = regexp.MustCompile(`^[0-9A-Z]{3}-[0-9A-Z]{4}-[0-9A-Z]{5}$`)
)

// NeweggURL builds a Newegg product URL from an item number (design doc,
// "Product URL"):
//
//   - 34-156-677      -> https://www.newegg.com/p/N82E16834156677
//   - 1TS-000E-1DFS6  -> https://www.newegg.com/p/1TS-000E-1DFS6
//   - 9SIC11UKGE3529  -> https://www.newegg.com/Product/Product.aspx?Item=9SIC11UKGE3529
//
// The marketplace form keeps its one query parameter: it is the item's
// identity, not tracking.
func NeweggURL(item string) (string, bool) {
	item = strings.ToUpper(strings.TrimSpace(item))
	if m := neweggNumeric.FindStringSubmatch(item); m != nil {
		return "https://www.newegg.com/p/N82E168" + m[1] + m[2] + m[3], true
	}
	if neweggMarket.MatchString(item) {
		return "https://www.newegg.com/Product/Product.aspx?Item=" + item, true
	}
	if neweggParent.MatchString(item) {
		return "https://www.newegg.com/p/" + item, true
	}
	return "", false
}

// Resolver follows tracked links by hand.
type Resolver struct {
	Client    *http.Client // must not follow redirects itself, no cookie jar
	UserAgent string
}

// canonical returns the clean product URL for u, when u is one: https, an
// allowed product host, a path matching the profile's productPath; no query,
// no fragment, no trailing slash.
func canonical(p *Profile, u *url.URL) (string, bool) {
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || !slices.Contains(p.ProductHosts, host) || p.pathRe == nil {
		return "", false
	}
	clean := path.Clean("/" + u.EscapedPath())
	if clean != "/" {
		clean = strings.TrimSuffix(clean, "/")
	}
	if !p.pathRe.MatchString(clean) {
		return "", false
	}
	segs := strings.Split(strings.Trim(clean, "/"), "/")
	for _, s := range segs[:len(segs)-1] {
		// A long digit or hex run in a slug segment is an id, possibly a
		// subscriber's: never part of a stored URL.
		if trackingSeg.MatchString(s) {
			return "", false
		}
	}
	return "https://" + host + clean, true
}

var trackingSeg = regexp.MustCompile(`[0-9]{8,}|[0-9a-f]{16,}`)

// MinSlugJaccard is the least Jaccard overlap of the slug's and the title's
// CONTENT tokens (see slugMatchesTitle for the one exception).
const MinSlugJaccard = 0.75

// slugStop are words that name nothing: articles and particles of wine
// names in several languages.
var slugStop = map[string]bool{
	"the": true, "by": true, "al": true, "du": true, "de": true, "di": true, "la": true, "le": true, "and": true,
	"of": true, "des": true, "del": true, "della": true, "el": true, "los": true, "las": true, "a": true, "et": true, "y": true, "e": true,
}

// slugGeneric are content words that alone identify no product: a slug made
// only of them ("red-wine", "red-blend") names a category page.
var slugGeneric = map[string]bool{
	"wine": true, "wines": true, "red": true, "white": true, "rose": true, "blend": true, "sparkling": true,
	"dessert": true, "gift": true, "set": true, "sale": true, "shop": true, "all": true,
}

// contentTokens are a text's tokens without stopwords and without anything
// carrying a digit (years and sizes are judged separately).
func contentTokens(s string) map[string]bool {
	out := map[string]bool{}
	for t := range tokens(s) {
		if slugStop[t] || sizeWords[t] || strings.ContainsAny(t, "0123456789") {
			continue
		}
		out[t] = true
	}
	return out
}

// sizeWords are compared by sizes(), never as content.
var sizeWords = map[string]bool{
	"ml": true, "l": true, "lt": true, "liter": true, "litre": true, "liters": true, "litres": true,
	"magnum": true, "half": true, "bottle": true, "pack": true, "case": true,
}

// variantMarkers name a DIFFERENT wine of the same producer and name (a
// rose, a riserva, an extra brut) or another format: a slug and a title
// that differ in one of them are different products.
var variantMarkers = map[string]bool{
	"rose": true, "riserva": true, "reserva": true, "reserve": true, "gran": true, "grand": true, "extra": true,
	"brut": true, "blanc": true, "noir": true, "magnum": true, "liter": true, "litre": true, "ml": true,
	"half": true, "pack": true, "case": true, "set": true, "sec": true, "demi": true, "doux": true, "nature": true,
}

// producerSuffixes may be on the slug and missing from the title ("caymus-
// vineyards-..." vs "Caymus ..."): they name the producer, not the wine.
var producerSuffixes = map[string]bool{
	"vineyards": true, "vineyard": true, "winery": true, "wines": true, "estate": true, "estates": true,
	"cellars": true, "cellar": true, "chateau": true, "domaine": true,
}

var (
	sizeMLRe   = regexp.MustCompile(`\b([0-9]+(?:[.-][0-9]+)?)\s*(ml|l|lt|liters?|litres?)\b`)
	sizePackRe = regexp.MustCompile(`\b([0-9]+)\s*-?\s*(?:pack|pk|bottle\s+case|case)\b`)
	sizeWordRe = regexp.MustCompile(`\b(magnum|half\s+bottle|half)\b`)
	// slugDecimal rejoins a decimal size a slug split on dashes ("1 5 liter").
	slugDecimal = regexp.MustCompile(`\b([0-9]+) ([0-9]+) (liter|litre|l)\b`)
)

// sizes is the set of bottle-size and pack markers in a text, normalized
// ("1-5-liter", "1.5L" and "1500ml" are all "1500ml"; "magnum" is
// "1500ml"; "half bottle" "375ml"); the standard 750ml bottle is no marker.
func sizes(s string) map[string]bool {
	out := map[string]bool{}
	s = strings.ToLower(s)
	for _, m := range sizeMLRe.FindAllStringSubmatch(s, -1) {
		v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], "-", "."), 64)
		if err != nil {
			continue
		}
		if m[2] != "ml" {
			v *= 1000
		}
		if ml := int(v + 0.5); ml != 750 {
			out[strconv.Itoa(ml)+"ml"] = true
		}
	}
	for _, m := range sizeWordRe.FindAllStringSubmatch(s, -1) {
		if m[1] == "magnum" {
			out["1500ml"] = true
		} else {
			out["375ml"] = true
		}
	}
	for _, m := range sizePackRe.FindAllStringSubmatch(s, -1) {
		if m[1] != "1" {
			out[m[1]+"pack"] = true
		}
	}
	return out
}

// otherNumbers are the numbers in a text that are neither a year nor part of
// a size or pack marker.
func otherNumbers(s string) map[string]bool {
	s = strings.ToLower(s)
	s = sizeMLRe.ReplaceAllString(s, " ")
	s = sizePackRe.ReplaceAllString(s, " ")
	s = yearRe.ReplaceAllString(s, " ")
	return numbers(s)
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// slugOf is the product slug of a canonical URL: the last path segment that
// is not the numeric id or a "p"/"product" marker (wine.com
// /product/<slug>/<id>, Total Wine /.../<slug>/p/<id>).
func slugOf(canonicalURL string) string {
	u, err := url.Parse(canonicalURL)
	if err != nil {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		s := segs[i]
		if s == "p" || s == "product" || allDigits(s) {
			continue
		}
		return s
	}
	return ""
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// slugMatchesTitle reports whether the product URL's slug names the product
// the image showed. A producer's other wines share most words with it, so:
//
//   - sizes (a magnum, 1.5 L, 375 ml, a 3-pack) must be the same on both
//     sides -- a size on one side only refuses; a year is compared when
//     both state one;
//   - the CONTENT tokens (stopwords, numbers and size words removed) must
//     not differ in any variant marker (rose, riserva, extra, brut, ...);
//   - they must have a Jaccard overlap of at least MinSlugJaccard, or differ
//     ONLY in one producer suffix on the slug side ("caymus-vineyards-...");
//   - the slug needs two content tokens, one of them not generic.
func slugMatchesTitle(canonicalURL, title string, slugHasVintage bool) bool {
	slugText := strings.ReplaceAll(slugOf(canonicalURL), "-", " ")
	// "1-5-liter" in a slug is 1.5 L: rejoin before splitting on dashes.
	sizeSlug := strings.ReplaceAll(slugOf(canonicalURL), "-", " ")
	sizeSlug = slugDecimal.ReplaceAllString(sizeSlug, "$1.$2 $3")
	if !sameSet(sizes(sizeSlug), sizes(title)) {
		return false
	}
	// A year is compared when both sides state one (Total Wine slugs carry
	// none). Where the retailer's slugs DO carry the vintage (wine.com), a
	// title year missing from the slug is another vintage's page.
	ys, yt := years(slugText), years(title)
	if len(ys) > 0 && len(yt) > 0 && !sameSet(ys, yt) {
		return false
	}
	if slugHasVintage && len(yt) > 0 && len(ys) == 0 {
		return false
	}
	// Other numbers (Penfolds Bin 389 vs Bin 707, a cuvee number) are
	// compared like years: when both sides carry some, they must be equal.
	if ns, nt := otherNumbers(sizeSlug), otherNumbers(title); len(ns) > 0 && len(nt) > 0 && !sameSet(ns, nt) {
		return false
	}
	cs, ct := contentTokens(slugText), contentTokens(title)
	distinctive := 0
	for t := range cs {
		if !slugGeneric[t] {
			distinctive++
		}
	}
	if len(cs) < 2 || distinctive == 0 || len(ct) == 0 {
		return false
	}
	inter := 0
	var slugOnly, titleOnly []string
	for t := range cs {
		if ct[t] {
			inter++
		} else {
			slugOnly = append(slugOnly, t)
		}
	}
	for t := range ct {
		if !cs[t] {
			titleOnly = append(titleOnly, t)
		}
	}
	for _, t := range append(slugOnly, titleOnly...) {
		if variantMarkers[t] {
			return false
		}
	}
	if float64(inter)/float64(len(cs)+len(ct)-inter) >= MinSlugJaccard {
		return true
	}
	return len(titleOnly) == 0 && len(slugOnly) == 1 && producerSuffixes[slugOnly[0]]
}

var yearRe = regexp.MustCompile(`\b(?:18|19|20)[0-9]{2}\b`)

func years(s string) map[string]bool {
	out := map[string]bool{}
	for _, y := range yearRe.FindAllString(s, -1) {
		out[y] = true
	}
	return out
}

// hopOK checks one URL of a chain: https, no userinfo, no port but 443, no
// IP literal, and a host the profile allows.
func hopOK(p *Profile, u *url.URL) string {
	if u.Scheme == "http" {
		return DropHTTPDowngrade
	}
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return DropLinkOffsite
	}
	host := strings.ToLower(u.Hostname())
	if _, err := netip.ParseAddr(host); err == nil {
		return DropLinkOffsite
	}
	if !p.hopAllowed(host) {
		return DropLinkOffsite
	}
	return ""
}

// Resolve finds a candidate's canonical product URL. A non-empty reason is
// a drop; temporary reports that a later poll may succeed.
func (r *Resolver) Resolve(ctx context.Context, p *Profile, c Candidate) (string, string, bool) {
	if p.Canonical == CanonicalNeweggItem {
		u, ok := NeweggURL(c.Item)
		if !ok {
			return "", DropBadItemNumber, false
		}
		return u, "", false
	}
	if c.Href == "" {
		return "", DropMissingLinkHref, false
	}
	cur, ok := upgradeURL(c.Href)
	if !ok || !slices.Contains(p.LinkHosts, cur.Host) {
		return "", DropLinkOffsite, false
	}
	seen := map[string]bool{}
	for hop := 0; ; hop++ {
		if why := hopOK(p, cur); why != "" {
			return "", why, false
		}
		if u, ok := canonical(p, cur); ok {
			// Reached the product URL: stop WITHOUT requesting the page.
			return u, "", false
		}
		if hop >= MaxHops {
			return "", DropTooManyHops, false
		}
		key := cur.String()
		if seen[key] {
			return "", DropRedirectLoop, false
		}
		seen[key] = true
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, key, nil)
		if err != nil {
			return "", DropLinkUnresolved, false
		}
		if r.UserAgent != "" {
			req.Header.Set("User-Agent", r.UserAgent)
		}
		resp, err := r.Client.Do(req)
		if err != nil {
			return "", DropLinkFailed, true
		}
		loc := resp.Header.Get("Location")
		code := resp.StatusCode
		drain(resp)
		switch {
		case isRedirect(code) && loc != "":
			next, err := cur.Parse(loc)
			if err != nil {
				return "", DropLinkUnresolved, false
			}
			cur = next
		case code >= 500 || code == http.StatusTooManyRequests:
			return "", DropLinkFailed, true
		default:
			// A page that is not a product URL (a landing page, a meta
			// refresh, an expired link): nothing to store.
			return "", DropLinkUnresolved, false
		}
	}
}
