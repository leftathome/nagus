package shopify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/listing"
)

// products.json is a REAL serverpartdeals.com page captured 2026-07-30 (5 real
// products spanning refurbished/new/unavailable/SSD/odd-prefix cases) plus ONE
// clearly-marked synthetic multi-variant product, because every product on the
// real page happened to carry a single variant.
const fixture = "testdata/products.json"

var fixedNow = time.Unix(1_750_000_000, 0).UTC()

func newTestConnector(t *testing.T, c Config) *Connector {
	t.Helper()
	if c.Name == "" {
		c.Name = "serverpartdeals"
	}
	if c.FixturePath == "" && c.BaseURL == "" {
		c.FixturePath = fixture
	}
	if c.Now == nil {
		c.Now = func() time.Time { return fixedNow }
	}
	return NewConnector(c)
}

func mustFetch(t *testing.T, c Config) []listing.Raw {
	t.Helper()
	raws, err := newTestConnector(t, c).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return raws
}

func TestSourceIDIsPerStore(t *testing.T) {
	if got, want := newTestConnector(t, Config{Name: "serverpartdeals"}).SourceID(), "shopify:serverpartdeals"; got != want {
		t.Fatalf("SourceID() = %q, want %q", got, want)
	}
	// Per-store identity is what gives isolated freshness/purge, so two stores
	// must never collide.
	a := newTestConnector(t, Config{Name: "storea"}).SourceID()
	b := newTestConnector(t, Config{Name: "storeb"}).SourceID()
	if a == b {
		t.Fatalf("two stores share a SourceID: %q", a)
	}
}

// Capacity is NOT in these product titles -- it lives in the capacity: tag and
// the product_type breadcrumb. Without a typed aspect every item would be
// dropped by the hdd capacity hard-filter, so this is the load-bearing mapping.
func TestCapacityTravelsAsTypedAspect(t *testing.T) {
	raws := mustFetch(t, Config{})
	if len(raws) == 0 {
		t.Fatal("fixture produced no rows")
	}
	for _, r := range raws {
		if strings.Contains(r.Title, "SYNTHETIC") {
			continue
		}
		// Proves the premise: the title alone would not yield a capacity.
		if strings.Contains(r.Title, "TB") {
			continue // a title that happens to carry one is fine, just not relied on
		}
		if r.Aspects["capacity_tb"] == "" {
			t.Errorf("no capacity_tb aspect for %q (title carries none either -- item would be filtered out)", r.Title)
		}
	}
}

func TestCapacityParsedFromTagAndBreadcrumb(t *testing.T) {
	cases := []struct {
		name string
		p    product
		want string
		ok   bool
	}{
		{"structured tag wins", product{Tags: []string{"brand:WD", "capacity:24TB"}, ProductType: "Hard Drives > 18TB > 3.5"}, "24", true},
		{"breadcrumb fallback", product{ProductType: "Hard Drives > 22TB > 3.5 > SAS > 7.2K RPM"}, "22", true},
		{"odd HDDs prefix", product{ProductType: "HDDs > 18TB > 3.5 > SATA > 7.2K RPM"}, "18", true},
		{"GB normalizes to TB", product{Tags: []string{"capacity:960GB"}}, "0.96", true},
		{"fractional TB", product{ProductType: "Solid State Drives > 3.84TB > 2.5 > SAS"}, "3.84", true},
		{"none", product{ProductType: "Accessories > Cables"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := capacityTB(tc.p)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("capacityTB = (%q,%v), want (%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// Condition is encoded in the tags, and new/refurbished are separate PRODUCTS.
func TestConditionFromTags(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{[]string{"000CardTitle:Refurbished Ultrastar DC HC580 24TB"}, "Refurbished"},
		{[]string{"000CardTitle:New Ultrastar DC HC580 24TB"}, "New"},
		{[]string{"brand:WD", "capacity:24TB"}, ""},
		{[]string{"condition:recertified"}, "Refurbished"},
	}
	for _, tc := range cases {
		if got := conditionFromTags(tc.tags); got != tc.want {
			t.Errorf("conditionFromTags(%v) = %q, want %q", tc.tags, got, tc.want)
		}
	}
}

// One Raw per VARIANT: each variant is separately priced and stocked.
func TestOneRawPerVariant(t *testing.T) {
	raws := mustFetch(t, Config{})
	var synth []listing.Raw
	for _, r := range raws {
		if strings.Contains(r.Title, "SYNTHETIC") {
			synth = append(synth, r)
		}
	}
	if len(synth) != 2 {
		t.Fatalf("multi-variant product produced %d rows, want 2 (one per variant)", len(synth))
	}
	if synth[0].SourceKey == synth[1].SourceKey {
		t.Fatalf("variants share SourceKey %q", synth[0].SourceKey)
	}
	if synth[0].PriceCents == synth[1].PriceCents {
		t.Errorf("variants should carry their own prices, both = %d", synth[0].PriceCents)
	}
	for _, r := range synth {
		if !strings.Contains(r.SourceKey, ":") {
			t.Errorf("SourceKey %q should be <productID>:<variantID>", r.SourceKey)
		}
	}
}

// Out-of-stock drives are not actionable deals; surfacing them is noise.
func TestUnavailableVariantsSkippedByDefault(t *testing.T) {
	def := mustFetch(t, Config{})
	all := mustFetch(t, Config{IncludeUnavailable: true})
	if len(all) <= len(def) {
		t.Fatalf("IncludeUnavailable did not widen the result set (%d vs %d); fixture may lack an unavailable row", len(all), len(def))
	}
}

// A mixed catalog (HDDs AND SSDs) must be filterable, and the filter has to cope
// with the inconsistent prefixes real catalogs use for one category.
func TestProductTypeAllowFilter(t *testing.T) {
	all := mustFetch(t, Config{})
	hdd := mustFetch(t, Config{ProductTypePrefixes: []string{"Hard Drives", "HDDs"}})
	if len(hdd) >= len(all) {
		t.Fatalf("allow-filter kept %d of %d rows, expected it to exclude the SSD", len(hdd), len(all))
	}
	if len(hdd) == 0 {
		t.Fatal("allow-filter excluded everything")
	}
	for _, r := range hdd {
		pt := strings.ToLower(r.Aspects["product_type"])
		if !strings.HasPrefix(pt, "hard drives") && !strings.HasPrefix(pt, "hdds") {
			t.Errorf("row %q slipped through with product_type %q", r.SourceKey, r.Aspects["product_type"])
		}
	}
	// Both spellings must be represented, else the test is not proving the point.
	var sawHardDrives, sawHDDs bool
	for _, r := range hdd {
		pt := strings.ToLower(r.Aspects["product_type"])
		sawHardDrives = sawHardDrives || strings.HasPrefix(pt, "hard drives")
		sawHDDs = sawHDDs || strings.HasPrefix(pt, "hdds")
	}
	if !sawHardDrives || !sawHDDs {
		t.Errorf("fixture should exercise BOTH prefixes; hard drives=%v hdds=%v", sawHardDrives, sawHDDs)
	}
}

func TestMappingBasics(t *testing.T) {
	raws := mustFetch(t, Config{BaseURL: "https://serverpartdeals.com", FixturePath: fixture})
	r := raws[0]
	if r.SourceID != "shopify:serverpartdeals" {
		t.Errorf("SourceID = %q", r.SourceID)
	}
	if !strings.HasPrefix(r.SourceURL, "https://serverpartdeals.com/products/") {
		t.Errorf("SourceURL = %q, want the storefront product permalink", r.SourceURL)
	}
	if r.Currency != "USD" {
		t.Errorf("Currency = %q", r.Currency)
	}
	if r.PriceCents <= 0 {
		t.Errorf("PriceCents = %d, want a real price", r.PriceCents)
	}
	if !r.SeenAt.Equal(fixedNow) {
		t.Errorf("SeenAt = %v, want %v", r.SeenAt, fixedNow)
	}
	if strings.Contains(r.Body, "<") {
		t.Errorf("Body still contains markup: %.80q", r.Body)
	}
}

func TestPriceCents(t *testing.T) {
	for in, want := range map[string]int64{"799.00": 79900, "269.00": 26900, "0.00": 0, "": 0, "junk": 0, "1234.56": 123456} {
		if got := priceCents(in); got != want {
			t.Errorf("priceCents(%q) = %d, want %d", in, got, want)
		}
	}
}

// A live storefront emitted a CP1252 byte (0x94, an inch mark) inside body_html,
// making the page invalid UTF-8. It must decode rather than fail the whole poll.
func TestDecodesInvalidUTF8Payload(t *testing.T) {
	bad := []byte(`{"products":[{"id":1,"title":"Drive","handle":"d","body_html":"3.5` + "\x94" + `","product_type":"Hard Drives > 8TB > 3.5","variants":[{"id":2,"price":"10.00","available":true}]}]}`)
	prods, err := decodeProducts(bad)
	if err != nil {
		t.Fatalf("decodeProducts on invalid UTF-8: %v", err)
	}
	if len(prods) != 1 {
		t.Fatalf("got %d products, want 1", len(prods))
	}
}

// --- network behavior ---------------------------------------------------------

func TestPaginationStopsOnShortPage(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		// One product => shorter than Limit => pagination must stop after page 1.
		_, _ = w.Write([]byte(`{"products":[{"id":1,"title":"D","handle":"d","product_type":"Hard Drives > 8TB > 3.5","variants":[{"id":2,"price":"10.00","available":true}]}]}`))
	}))
	defer srv.Close()
	c := NewConnector(Config{Name: "s", BaseURL: srv.URL, Limit: 250, Now: func() time.Time { return fixedNow }})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if pages != 1 {
		t.Errorf("requested %d pages, want 1 (short page ends pagination)", pages)
	}
	if len(raws) != 1 {
		t.Errorf("got %d raws, want 1", len(raws))
	}
}

func TestRateLimitIsIdentifiable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("local_rate_limited"))
	}))
	defer srv.Close()
	c := NewConnector(Config{Name: "s", BaseURL: srv.URL})
	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("want an error on 429")
	}
	if !errorsIs(err, ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
}

func TestNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<!DOCTYPE html><html>nope</html>"))
	}))
	defer srv.Close()
	c := NewConnector(Config{Name: "s", BaseURL: srv.URL})
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("want an error on 404")
	}
}

func TestUserAgentIsSent(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"products":[]}`))
	}))
	defer srv.Close()
	if _, err := NewConnector(Config{Name: "s", BaseURL: srv.URL}).Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.HasPrefix(ua, "nagus/") {
		t.Errorf("User-Agent = %q, want a nagus identifier", ua)
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// A multi-capacity product sells several capacities under one listing, with the
// capacity on the VARIANT. Each Raw must report its own variant's capacity, not
// the product-level one, or a 16TB variant would be scored as the product's 12TB.
func TestMultiVariantCapacityIsPerVariant(t *testing.T) {
	raws := mustFetch(t, Config{})
	got := map[string]string{}
	for _, r := range raws {
		if strings.Contains(r.Title, "SYNTHETIC") {
			got[r.SourceKey] = r.Aspects["capacity_tb"]
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 synthetic variant rows, got %d", len(got))
	}
	var caps []string
	for _, v := range got {
		caps = append(caps, v)
	}
	if caps[0] == caps[1] {
		t.Fatalf("both variants report capacity %q; each must report its own (12 and 16)", caps[0])
	}
	for _, want := range []string{"12", "16"} {
		found := false
		for _, v := range got {
			if v == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no variant reported capacity %q; got %v", want, got)
		}
	}
}
