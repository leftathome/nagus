// Package promo reads image-only retailer promo mail (nagus-3ua; design
// docs/design/2026-10-16-promo-vision.md). A retailer newsletter whose
// products are pictures -- Newegg tiles, wine.com and Total Wine images
// rendered at open time -- carries no text a parser could read. This package
// picks the product images out of the mail's HTML, fetches them from the
// retailer's own image hosts, reads each one TWICE with an OpenAI-compatible
// vision model, keeps a product only when both reads agree on its price,
// resolves a clean canonical product URL, and hands the result to the
// nagus.deal/v1 validator as if a household member had typed it.
//
// Everything a mail or an image says is UNTRUSTED. The vision model's answer
// is parsed strictly into a fixed shape; the worst a hostile image can do is
// produce a wrong field value, which the agreement guard, the deal validator
// and the glovebox gate then judge like any other listing.
package promo

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ParserName is the imapParser value that selects this parser.
const ParserName = "promo-vision-v1"

// Canonical rules: how a product's URL is found.
const (
	// CanonicalNeweggItem builds the URL from the Newegg item number in the
	// image's alt text; the tracked link is never requested.
	CanonicalNeweggItem = "newegg-item"
	// CanonicalFollowLink follows the image's enclosing tracked link, once.
	CanonicalFollowLink = "follow-link"
)

// DefaultMaxImages caps the images read from one message.
const DefaultMaxImages = 24

// Profile is one retailer's rules. Profiles are DATA (profiles.json); the
// code knows only the two canonical rules.
type Profile struct {
	Name     string `json:"name"`
	Seller   string `json:"seller"`
	Category string `json:"category"`
	// SenderDomains: every sender of a source using this profile must be on
	// or under one of these.
	SenderDomains []string `json:"senderDomains"`
	// ImageHosts are the only hosts an image is fetched from.
	ImageHosts []string `json:"imageHosts"`
	// ProductAlt, when set, admits only images whose alt matches; its first
	// group is the retailer's item number.
	ProductAlt string `json:"productAlt,omitempty"`
	// LinkHosts are the tracked redirectors a resolution may start at.
	LinkHosts []string `json:"linkHosts"`
	// RedirectHosts are further hosts a redirect chain may pass through.
	RedirectHosts []string `json:"redirectHosts,omitempty"`
	// ProductHosts and ProductPath: what a product URL must look like.
	ProductHosts []string `json:"productHosts"`
	ProductPath  string   `json:"productPath,omitempty"`
	Canonical    string   `json:"canonical"`
	// SlugHasVintage: the retailer's product slugs carry the vintage, so a
	// title's year must appear in the slug (wine.com).
	SlugHasVintage bool `json:"slugHasVintage,omitempty"`
	// AllowSluglessLinks accepts a resolved product URL that carries no name
	// slug (wine.com /product/<id>, Newegg /p/<item>): the link cannot be
	// checked against the image, so the offer is marked link not
	// name-verified. Without it such a link is dropped.
	AllowSluglessLinks bool `json:"allowSluglessLinks,omitempty"`
	MaxImages          int  `json:"maxImages,omitempty"`

	altRe  *regexp.Regexp
	pathRe *regexp.Regexp
}

//go:embed profiles.json
var profilesJSON []byte

var builtin = mustLoad(profilesJSON)

func mustLoad(b []byte) map[string]*Profile {
	ps, err := LoadProfiles(b)
	if err != nil {
		panic("promo: built-in profiles: " + err.Error())
	}
	return ps
}

// LoadProfiles parses and validates a profiles document.
func LoadProfiles(b []byte) (map[string]*Profile, error) {
	var doc struct {
		Profiles []*Profile `json:"profiles"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	out := map[string]*Profile{}
	for _, p := range doc.Profiles {
		if err := p.init(); err != nil {
			return nil, fmt.Errorf("profile %q: %w", p.Name, err)
		}
		if _, dup := out[p.Name]; dup {
			return nil, fmt.Errorf("profile %q defined twice", p.Name)
		}
		out[p.Name] = p
	}
	return out, nil
}

func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (p *Profile) init() error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || p.Seller == "" {
		return fmt.Errorf("name and seller are required")
	}
	p.SenderDomains, p.ImageHosts = lowerAll(p.SenderDomains), lowerAll(p.ImageHosts)
	p.LinkHosts, p.RedirectHosts, p.ProductHosts = lowerAll(p.LinkHosts), lowerAll(p.RedirectHosts), lowerAll(p.ProductHosts)
	if len(p.SenderDomains) == 0 || len(p.ImageHosts) == 0 || len(p.ProductHosts) == 0 {
		return fmt.Errorf("senderDomains, imageHosts and productHosts are required")
	}
	if p.ProductAlt != "" {
		re, err := regexp.Compile(p.ProductAlt)
		if err != nil {
			return fmt.Errorf("productAlt: %w", err)
		}
		p.altRe = re
	}
	switch p.Canonical {
	case CanonicalNeweggItem:
		if p.altRe == nil || p.altRe.NumSubexp() < 1 {
			return fmt.Errorf("canonical %s needs a productAlt with an item-number group", p.Canonical)
		}
		if p.ProductPath != "" && len(p.LinkHosts) == 0 {
			return fmt.Errorf("canonical %s with a productPath (the tracked-link fallback) needs linkHosts", p.Canonical)
		}
	case CanonicalFollowLink:
		if len(p.LinkHosts) == 0 || p.ProductPath == "" {
			return fmt.Errorf("canonical %s needs linkHosts and productPath", p.Canonical)
		}
	default:
		return fmt.Errorf("canonical %q (want %s or %s)", p.Canonical, CanonicalNeweggItem, CanonicalFollowLink)
	}
	if p.ProductPath != "" {
		re, err := regexp.Compile(p.ProductPath)
		if err != nil {
			return fmt.Errorf("productPath: %w", err)
		}
		p.pathRe = re
	}
	if p.MaxImages <= 0 {
		p.MaxImages = DefaultMaxImages
	}
	return nil
}

// Lookup returns a built-in profile by name.
func Lookup(name string) (*Profile, error) {
	if p, ok := builtin[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("promo: no profile named %q (built in: %v)", name, ProfileNames())
}

// ProfileNames lists the built-in profiles, sorted.
func ProfileNames() []string {
	out := make([]string, 0, len(builtin))
	for n := range builtin {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// onOrUnder reports whether host is d or a subdomain of d.
func onOrUnder(host, d string) bool {
	return host == d || strings.HasSuffix(host, "."+d)
}

// SenderAllowed reports whether a sender address is on or under one of the
// profile's sender domains.
func (p *Profile) SenderAllowed(addr string) bool {
	addr = strings.ToLower(strings.TrimSpace(addr))
	i := strings.LastIndex(addr, "@")
	if i < 0 {
		return false
	}
	dom := addr[i+1:]
	for _, d := range p.SenderDomains {
		if onOrUnder(dom, d) {
			return true
		}
	}
	return false
}

// Principal is the name stamped on this retailer's offers.
func (p *Profile) Principal() string { return "retailer:" + p.Name }

// hopAllowed is every host a link resolution may visit.
func (p *Profile) hopAllowed(host string) bool {
	return slices.Contains(p.LinkHosts, host) || slices.Contains(p.RedirectHosts, host) || slices.Contains(p.ProductHosts, host)
}

// itemNumber returns the retailer item number in an alt text, or "".
func (p *Profile) itemNumber(alt string) string {
	if p.altRe == nil {
		return ""
	}
	m := p.altRe.FindStringSubmatch(strings.TrimSpace(alt))
	if m == nil {
		return ""
	}
	return m[1]
}
