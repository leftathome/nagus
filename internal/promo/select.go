package promo

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Candidate is one product image picked out of a message's HTML. Src and
// Href are the mail's own URLs and may carry tracking tokens: they are used
// for one request each and NEVER stored, logged or returned.
type Candidate struct {
	Src  string // the image URL, upgraded to https
	Alt  string
	Href string // the enclosing link, "" when none
	Item string // the retailer item number from the alt, when the profile has one
}

// MinDimension is the smallest width or height hint (and decoded size) an
// image may have; pixels and spacers are smaller.
const MinDimension = 60

// navAlt matches alt texts of navigation, branding and footer images.
var navAlt = regexp.MustCompile(`(?i)\b(?:logo|facebook|twitter|instagram|youtube|tiktok|pinterest|linkedin|snapchat|app ?store|google play|download|get (?:it|our app)|preferences|unsubscribe|customer care|privacy|social|view in browser|header|footer|spacer|pixel)\b|^\s*x\s*$`)

var (
	styleWidth  = regexp.MustCompile(`(?i)(?:^|[;\s])width\s*:\s*([0-9]+)px`)
	styleHeight = regexp.MustCompile(`(?i)(?:^|[;\s])height\s*:\s*([0-9]+)px`)
	styleHidden = regexp.MustCompile(`(?i)display\s*:\s*none|visibility\s*:\s*hidden`)
)

// Selection reports what SelectImages saw.
type Selection struct {
	Seen       int
	Candidates []Candidate
	Skipped    map[string]int // reason -> count
}

func (s *Selection) skip(reason string) {
	if s.Skipped == nil {
		s.Skipped = map[string]int{}
	}
	s.Skipped[reason]++
}

// Skip reasons at selection.
const (
	SkipImageHost  = "image_host_not_allowed"
	SkipHidden     = "hidden"
	SkipTiny       = "tiny"
	SkipNav        = "nav_or_logo"
	SkipNoItem     = "no_item_number"
	SkipNoLink     = "no_product_link"
	SkipDuplicate  = "duplicate"
	SkipOverCap    = "over_cap"
	SkipBadImgURL  = "bad_image_url"
	SkipNoImageSrc = "no_src"
)

// dimHint reads a width or height hint from an attribute or inline style;
// 0 when there is none ("auto", "100%").
func dimHint(attr string, style string, re *regexp.Regexp) int {
	if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(attr), "px")); err == nil {
		return n
	}
	if m := re.FindStringSubmatch(style); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// upgradeURL parses an absolute http(s) URL and returns it as https, with
// its host lower-cased. It refuses anything else.
func upgradeURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Host == "" {
		return nil, false
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		if p := u.Port(); p != "" && p != "80" {
			return nil, false
		}
	case "https":
		if p := u.Port(); p != "" && p != "443" {
			return nil, false
		}
	default:
		return nil, false
	}
	u.Scheme = "https"
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // an IPv6 literal keeps its brackets
	}
	u.Host = host
	return u, true
}

// SelectImages picks a message's product images for profile p.
func SelectImages(doc string, p *Profile) Selection {
	var sel Selection
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return sel
	}
	seen := map[string]bool{}
	// Tiles that carry their own item number fill the cap first; images
	// found only through their tracked link (no item number on a profile
	// that has one) come after, so banners never push real tiles over the
	// cap or ahead in the vision budget. Document order is kept within each.
	var primary, fallback []Candidate
	var walk func(n *html.Node, href string)
	walk = func(n *html.Node, href string) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "a":
				href = attr(n, "href")
			case "img":
				sel.Seen++
				if c, why := candidate(n, href, p); why != "" {
					sel.skip(why)
				} else if seen[c.Src] {
					sel.skip(SkipDuplicate)
				} else {
					seen[c.Src] = true
					if p.altRe != nil && c.Item == "" {
						fallback = append(fallback, c)
					} else {
						primary = append(primary, c)
					}
				}
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch, href)
		}
	}
	walk(root, "")
	for _, c := range append(primary, fallback...) {
		if len(sel.Candidates) >= p.MaxImages {
			sel.skip(SkipOverCap)
			continue
		}
		sel.Candidates = append(sel.Candidates, c)
	}
	return sel
}

func candidate(n *html.Node, href string, p *Profile) (Candidate, string) {
	src := attr(n, "src")
	if src == "" {
		return Candidate{}, SkipNoImageSrc
	}
	u, ok := upgradeURL(src)
	if !ok {
		return Candidate{}, SkipBadImgURL
	}
	if !slices.Contains(p.ImageHosts, u.Host) {
		return Candidate{}, SkipImageHost
	}
	style := attr(n, "style")
	if styleHidden.MatchString(style) {
		return Candidate{}, SkipHidden
	}
	w, h := dimHint(attr(n, "width"), style, styleWidth), dimHint(attr(n, "height"), style, styleHeight)
	if (w > 0 && w < MinDimension) || (h > 0 && h < MinDimension) || attr(n, "height") == "0" || attr(n, "width") == "0" {
		return Candidate{}, SkipTiny
	}
	alt := strings.TrimSpace(attr(n, "alt"))
	if navAlt.MatchString(alt) {
		return Candidate{}, SkipNav
	}
	c := Candidate{Src: u.String(), Alt: alt}
	if p.altRe != nil {
		c.Item = p.itemNumber(alt)
		if c.Item == "" && p.pathRe == nil {
			return Candidate{}, SkipNoItem
		}
	}
	// Without an item number the product is found by following the
	// tile's tracked link (Newegg mails that dropped the item_ alt).
	if p.Canonical == CanonicalFollowLink || c.Item == "" && p.altRe != nil {
		lu, ok := upgradeURL(href)
		if !ok || !slices.Contains(p.LinkHosts, lu.Host) {
			return Candidate{}, SkipNoLink
		}
		c.Href = lu.String()
	}
	return c, ""
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}
