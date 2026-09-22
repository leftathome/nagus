// Package fingerprint classifies a winery or distillery web store by the
// e-commerce platform behind it, and finds the machine-readable endpoint nagus
// would ingest from (nagus-b0u).
//
// It exists because the platform decides the connector: a Shopify store has a
// public /products.json, a Vinoshipper producer a public wine-list feed keyed by
// an account id, an OrderPort store a regular HTML catalogue, a Commerce7 store
// an auth-gated API behind a rendered storefront. Adding a source starts with
// knowing which of these it is, and the answer is rarely on the marketing page.
//
// It is polite by construction: one request at a time, a pause between them, a
// descriptive User-Agent, robots.txt read first and reported. It only ever
// GETs public pages; it never logs in, submits forms, or follows age gates.
package fingerprint

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Platform names the storefront technology.
type Platform string

// Platforms in the order they are preferred when several signals appear (a
// winery's WordPress site embedding a Vinoshipper catalogue is a Vinoshipper
// source, not a WordPress one).
const (
	Shopify     Platform = "shopify"
	Vinoshipper Platform = "vinoshipper"
	Commerce7   Platform = "commerce7"
	OrderPort   Platform = "orderport"
	WooCommerce Platform = "woocommerce"
	AMS         Platform = "ams-ecellar"
	WineDirect  Platform = "winedirect-vin65"
	Squarespace Platform = "squarespace"
	WordPress   Platform = "wordpress"
	Unknown     Platform = "unknown"
)

// Result is what one domain turned out to be.
type Result struct {
	URL      string   `json:"url"`
	Platform Platform `json:"platform"`
	// Signals are the markers that were seen, for a human to check the call.
	Signals []string `json:"signals"`
	// Endpoint is what a connector would fetch, when one was found.
	Endpoint string `json:"endpoint,omitempty"`
	// VinoshipperAccount is the producer's Vinoshipper account id (0 = none).
	VinoshipperAccount int `json:"vinoshipper_account,omitempty"`
	// Commerce7Tenant is the Commerce7 tenant slug, when discoverable.
	Commerce7Tenant string `json:"commerce7_tenant,omitempty"`
	// OrderPortHost is the store's orderport.net host.
	OrderPortHost string `json:"orderport_host,omitempty"`
	// ProductJSONLD reports schema.org Product JSON-LD on the fetched page.
	ProductJSONLD bool `json:"product_jsonld,omitempty"`
	// RobotsDisallow lists robots.txt Disallow rules that touch catalogue paths.
	RobotsDisallow []string `json:"robots_disallow,omitempty"`
	Error          string   `json:"error,omitempty"`
}

// Prober fetches and classifies. The zero value is usable.
type Prober struct {
	HTTP      *http.Client
	UserAgent string
	// Pause is the delay between requests to one host; 0 = one second.
	Pause time.Duration
	// MaxBody caps each response read; 0 = 4 MiB.
	MaxBody int64
}

const defaultUserAgent = "nagus-fingerprint/1 (+personal deal tracker; polite, one request at a time)"

var (
	reVinoInit     = regexp.MustCompile(`Vinoshipper\.init\(\s*(\d+)`)
	reVinoWineList = regexp.MustCompile(`json-api/v2/wine-list\?id=(\d+)`)
	reVinoData     = regexp.MustCompile(`data-vs-(?:list|account|producer)="(\d+)"`)
	reC7Tenant     = []*regexp.Regexp{
		regexp.MustCompile(`([a-z0-9-]+)\.admin\.platform\.commerce7\.com`),
		regexp.MustCompile(`(?i)["']?tenant(?:Id)?["']?\s*[:=]\s*["']([a-z0-9-]+)["']`),
		regexp.MustCompile(`data-tenant(?:-id)?="([a-z0-9-]+)"`),
	}
	reOrderPort = regexp.MustCompile(`([a-z0-9-]+\.orderport\.net)`)
	reJSONLD    = regexp.MustCompile(`(?is)<script[^>]*application/ld\+json[^>]*>(.*?)</script>`)
)

// Probe classifies one site. rawURL may be a bare domain.
func (p *Prober) Probe(ctx context.Context, rawURL string) Result {
	base, err := normalize(rawURL)
	if err != nil {
		return Result{URL: rawURL, Platform: Unknown, Error: err.Error()}
	}
	res := Result{URL: base.String(), Platform: Unknown}
	signal := func(s string) { res.Signals = append(res.Signals, s) }

	// robots.txt first: what the site asks, reported whatever we find.
	if body, code, err := p.get(ctx, base.ResolveReference(&url.URL{Path: "/robots.txt"}).String()); err == nil && code == http.StatusOK {
		res.RobotsDisallow = catalogueDisallows(body)
	}

	// Shopify: a working /products.json settles it.
	pj := base.ResolveReference(&url.URL{Path: "/products.json", RawQuery: "limit=1"}).String()
	if body, code, err := p.get(ctx, pj); err == nil && code == http.StatusOK && isShopifyProducts(body) {
		signal("products.json answered")
		res.Platform, res.Endpoint = Shopify, base.ResolveReference(&url.URL{Path: "/products.json"}).String()
	}

	home, code, err := p.get(ctx, base.String())
	if err != nil || code != http.StatusOK {
		if res.Platform == Unknown {
			if err == nil {
				err = fmt.Errorf("homepage HTTP %d", code)
			}
			res.Error = err.Error()
		}
		return res
	}
	h := strings.ToLower(home)
	markers := map[Platform]bool{}
	mark := func(pl Platform, s string) { markers[pl] = true; signal(s) }

	if strings.Contains(h, "cdn.shopify.com") || strings.Contains(h, "cdn/shop/") || strings.Contains(h, "shopify.theme") {
		mark(Shopify, "shopify assets")
	}
	if m := firstMatch(home, reVinoInit, reVinoWineList, reVinoData); m != "" {
		res.VinoshipperAccount, _ = strconv.Atoi(m)
		mark(Vinoshipper, "vinoshipper account "+m)
	} else if strings.Contains(h, "vinoshipper") {
		mark(Vinoshipper, "vinoshipper reference (no account id on this page)")
	}
	if strings.Contains(h, "commerce7") || strings.Contains(h, "c7-content") || strings.Contains(h, "c7wp_settings") {
		mark(Commerce7, "commerce7 markers")
		res.Commerce7Tenant = firstMatch(home, reC7Tenant...)
	}
	if m := reOrderPort.FindString(h); m != "" {
		res.OrderPortHost = m
		mark(OrderPort, "orderport host "+m)
	}
	if strings.Contains(h, ".ams") && (strings.Contains(h, "login.ams") || strings.Contains(h, "/cart.ams") || strings.Contains(h, ".ams\"")) || strings.Contains(h, "ecellar") {
		mark(AMS, "ams/ecellar paths")
	}
	if strings.Contains(h, "v65-") || strings.Contains(h, "vin65") {
		mark(WineDirect, "vin65 markers")
	}
	if strings.Contains(h, "static1.squarespace.com") {
		mark(Squarespace, "squarespace assets")
	}
	if strings.Contains(h, "wp-content") {
		mark(WordPress, "wordpress")
		wc := base.ResolveReference(&url.URL{Path: "/wp-json/wc/store/v1/products", RawQuery: "per_page=1"}).String()
		if body, code, err := p.get(ctx, wc); err == nil && code == http.StatusOK && strings.HasPrefix(strings.TrimSpace(body), "[") {
			mark(WooCommerce, "woocommerce store api answered")
		}
	}
	res.ProductJSONLD = hasProductJSONLD(home)
	if res.ProductJSONLD {
		signal("product json-ld")
	}

	if res.Platform == Unknown {
		for _, pl := range []Platform{Shopify, Vinoshipper, Commerce7, OrderPort, WooCommerce, AMS, WineDirect, Squarespace, WordPress} {
			if markers[pl] {
				res.Platform = pl
				break
			}
		}
	}
	switch res.Platform {
	case Vinoshipper:
		if res.VinoshipperAccount > 0 {
			res.Endpoint = fmt.Sprintf("https://vinoshipper.com/json-api/v2/wine-list?id=%d", res.VinoshipperAccount)
		}
	case OrderPort:
		res.Endpoint = "https://" + res.OrderPortHost + "/wines/All-Wines"
	case WooCommerce:
		res.Endpoint = base.ResolveReference(&url.URL{Path: "/wp-json/wc/store/v1/products"}).String()
	}
	return res
}

func (p *Prober) get(ctx context.Context, u string) (string, int, error) {
	pause := p.Pause
	if pause == 0 {
		pause = time.Second
	}
	select {
	case <-ctx.Done():
		return "", 0, ctx.Err()
	case <-time.After(pause):
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, err
	}
	ua := p.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	max := p.MaxBody
	if max <= 0 {
		max = 4 << 20
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max))
	return string(b), resp.StatusCode, err
}

func normalize(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("not a URL or domain: %q", raw)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/"}, nil
}

func isShopifyProducts(body string) bool {
	var v struct {
		Products []json.RawMessage `json:"products"`
	}
	return json.Unmarshal([]byte(body), &v) == nil && v.Products != nil
}

func hasProductJSONLD(page string) bool {
	for _, m := range reJSONLD.FindAllStringSubmatch(page, -1) {
		if regexp.MustCompile(`"@type"\s*:\s*"Product"`).MatchString(m[1]) {
			return true
		}
	}
	return false
}

func catalogueDisallows(robots string) []string {
	var out []string
	for _, line := range strings.Split(robots, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(l), "disallow:") {
			continue
		}
		path := strings.TrimSpace(l[len("disallow:"):])
		lp := strings.ToLower(path)
		if path == "/" || strings.Contains(lp, "product") || strings.Contains(lp, "json") ||
			strings.Contains(lp, "wine") || strings.Contains(lp, "shop") || strings.Contains(lp, "collection") {
			out = append(out, path)
		}
	}
	return out
}

func firstMatch(s string, res ...*regexp.Regexp) string {
	for _, re := range res {
		if m := re.FindStringSubmatch(s); m != nil {
			return m[1]
		}
	}
	return ""
}
