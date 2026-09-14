package shopify_test

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/leftathome/nagus/internal/connector/shopify"
	"github.com/leftathome/nagus/internal/offer"
)

// THE CROSS-SELLER DEDUP BASELINE, reproducible from committed data.
//
// nagus-ejk recorded "250 offers -> 132 distinct products, 59 groups" from a
// capture that was never committed, so that number could not be re-derived by
// anyone, and quark's slice-1 acceptance (a) was pinned to it. The original data
// is gone and the live catalogue has changed, so the baseline is RE-DERIVED
// here from a capture that IS committed, and quark's parity check is pinned to
// THESE numbers instead.
//
// FIXTURE: testdata/serverpartdeals_2026-09-12.json -- ONE request to
// /products.json?limit=250&page=1 on 2026-09-12, no redirects followed. Trimmed
// to the fields this connector decodes; body_html is blanked (the retailer's
// authored copy -- it becomes offer body text and never feeds a hint, so the
// counts below are unaffected). Never refresh it by re-fetching: a fresh
// capture is a different catalogue, which is a new baseline, not a fix.
//
// CONFIG: exactly the production serverpartdeals source (gitops
// clusters/orac/apps/nagus/helmrelease-nagus.yaml): the HDD allow-filter, brand
// from the "brand:" tag, SKU-is-MPN, and the _SR/_MR/_NB condition suffixes
// stripped. Change the production config and this test is the thing that says
// what it did to grouping.
//
// ONE DELIBERATE DIFFERENCE FROM PRODUCTION: IncludeUnavailable is true.
// Identity is a property of the product, not of today's stock. This store lists
// each drive once per condition (_SR/_MR/_NB) and usually only one condition is
// in stock, so the production filter on this page yields 22 offers and ZERO
// multi-offer groups -- a parity check over that would pass while testing
// nothing about dedup. Over every HDD listing the page holds 104 offers that
// group into 35 drives, every one of them listed in more than one condition.
const (
	baselineFixture = "testdata/serverpartdeals_2026-09-12.json"
	baselineHints   = "testdata/serverpartdeals_2026-09-12.hints.json"
)

var updateHints = flag.Bool("update-hints", false, "rewrite "+baselineHints+" (the file copied into quark's testdata)")

// Pinned from the committed fixture. See the test for what each counts.
const (
	wantOffers            = 104 // every HDD listing on the page, in stock or not
	wantKeyedOffers       = 104 // brand: tag + SKU-as-MPN on all of them
	wantDistinctProducts  = 35
	wantMultiOfferGroups  = 35 // every drive is listed in 2+ conditions
	wantNonASCIIHintCount = 0  // so quark's gate 1 has no grounds to diverge
)

// exportedHint is the wire shape quark's POST /resolve accepts (quark Hint),
// in fixture order. quark's parity test resolves exactly these.
type exportedHint struct {
	Category string `json:"category"`
	Brand    string `json:"brand,omitempty"`
	MPN      string `json:"mpn,omitempty"`
	GTIN     string `json:"gtin,omitempty"`
	Model    string `json:"model,omitempty"`
}

func TestServerpartdealsDedupBaseline(t *testing.T) {
	c := shopify.NewConnector(shopify.Config{
		Name:                "serverpartdeals",
		FixturePath:         baselineFixture,
		ProductTypePrefixes: []string{"Hard Drives", "HDDs"},
		BrandTag:            "brand:",
		SKUIsMPN:            true,
		SKUSuffixes:         []string{"_SR", "_MR", "_NB"},
		IncludeUnavailable:  true,
		Now:                 func() time.Time { return time.Unix(1_788_000_000, 0).UTC() },
	})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	groups := map[string]int{}
	keyed, nonASCII := 0, 0
	hints := make([]exportedHint, 0, len(raws))
	for _, r := range raws {
		h := offer.ProductHint{
			Brand: r.Aspects["brand"],
			MPN:   r.Aspects["mpn"],
			GTIN:  r.Aspects["gtin"],
			Model: r.Aspects["model"],
		}
		// Spec (quark section 11, slice 1): scan hint fields for non-ASCII
		// BEFORE asserting parity. provisionalKey (below) silently deletes such
		// runes and groups the offer; quark's gate 1 refuses it. Every such hint
		// is a place the two algorithms are allowed to disagree.
		for _, s := range []string{h.Brand, h.MPN, h.GTIN, h.Model} {
			if hasNonASCII(s) {
				nonASCII++
				t.Logf("non-ASCII hint field in %q: %q", r.SourceKey, s)
				break
			}
		}
		hints = append(hints, exportedHint{Category: "hdd", Brand: h.Brand, MPN: h.MPN, GTIN: h.GTIN, Model: h.Model})
		if k := provisionalKey(h); k != "" {
			keyed++
			groups[k]++
		}
	}
	multi := 0
	for _, n := range groups {
		if n > 1 {
			multi++
		}
	}

	t.Logf("offers=%d keyed=%d distinct=%d multi-offer groups=%d non-ASCII hints=%d",
		len(raws), keyed, len(groups), multi, nonASCII)

	if *updateHints {
		b, err := json.MarshalIndent(hints, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(baselineHints, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s; copy it to quark internal/product/testdata/", baselineHints)
	}

	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"offers", len(raws), wantOffers},
		{"offers carrying a provisional key", keyed, wantKeyedOffers},
		{"distinct products", len(groups), wantDistinctProducts},
		{"groups holding more than one offer", multi, wantMultiOfferGroups},
		{"hints with non-ASCII fields", nonASCII, wantNonASCIIHintCount},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d. The fixture is frozen, so a change here is a change "+
				"to the connector's hint emission or to provisionalKey: explain it, "+
				"and move quark's parity number with it.", c.name, c.got, c.want)
		}
	}
}

func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return false
}

// provisionalKey is a TEST-LOCAL, FROZEN copy of the deleted
// offer.ComputeProvisionalKey (nagus main 7778b807). Production no longer
// groups offers locally -- quark's product id does (spec D4) -- but quark's
// parity acceptance number (104 hints -> 35 products) was derived with this
// exact algorithm, so it stays here to keep that number reproducible. Do not
// improve it: changing it moves a baseline, not a feature.
func provisionalKey(h offer.ProductHint) string {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	if mpn := norm(h.MPN); mpn != "" {
		return "mpn:" + mpn
	}
	brand, model := norm(h.Brand), norm(h.Model)
	if brand != "" && model != "" {
		return "bm:" + brand + ":" + model
	}
	return ""
}
