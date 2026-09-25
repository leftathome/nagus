package main

import (
	"context"
	"testing"
	"time"

	extwine "github.com/leftathome/nagus/internal/extract/wine"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
)

// Wine rows compare on the key quark's vintage_mode implies, not on product id
// alone (quark QUARK-04): a non-vintage blend groups across whatever year its
// titles mention, vintages of one wine stay apart, and a vintage wine listed
// with no year is compared with nothing. Realistic titles go through the real
// extractor, the real offer store and the real read-path stamping.
func TestWineRowsCompareOnVintageAwareKeys(t *testing.T) {
	ctx := context.Background()
	offers := offer.NewMemoryStore()
	srv := &server{offers: offers}

	type listingCase struct {
		key, title, wineType, productID, mode string
	}
	cases := []listingCase{
		// No product type: as a Shopify listing arrives (its product_type
		// is not wine_type). NV beside "Brut", and the disgorgement, are the
		// wine evidence.
		{"bsc-nv", "Bollinger Special Cuvee Brut NV", "", "p-special-cuvee", "non_vintage"},
		{"bsc-2019", "Bollinger Special Cuvee (disgorged 2019)", "", "p-special-cuvee", "non_vintage"},
		{"lga-2014", "Bollinger La Grande Annee 2014", "", "p-grande-annee", "vintage"},
		{"lga-2015", "Bollinger La Grande Annee 2015", "", "p-grande-annee", "vintage"},
		// A vintage-mode product whose listing says NV: a conflict, no key,
		// and the disgorgement year is not taken as the vintage.
		{"conflict", "Bollinger Special Cuvee Brut NV, disgorged 2019", "White", "p-grande-annee", "vintage"},
		// quark does not know the mode; the listing's NV decides (needs the
		// "nv" row detail on the read path).
		{"unknown-nv", "Hypothetical Cuvee Brut NV", "White", "p-hypothetical", "unknown"},
		// A founding year in a producer name is not a vintage; a bottling
		// date after the vintage is ignored.
		{"est", "Old Winery Est. 1970 Cabernet Sauvignon", "Red", "p-old-winery", "vintage"},
		{"opus-bottled", "Opus One 2019 (bottled 2021)", "Red", "p-opus-one", "vintage"},
		{"opus-2019", "Opus One 2019", "Red", "p-opus-one", "vintage"},
		{"opus-none", "Opus One", "Red", "p-opus-one", "vintage"},
	}
	var rows []searchRow
	for _, c := range cases {
		it, err := extwine.New().Extract(ctx, listing.Sanitized{
			SourceID: "shopify:store", SourceKey: c.key, Title: c.title, PriceCents: 10000, Currency: "USD",
			Aspects: map[string]string{"wine_type": c.wineType}, SeenAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("%q did not extract: %v", c.title, err)
		}
		o := offer.Offer{SourceID: "shopify:store", SourceKey: c.key, PriceCents: 10000, LastSeen: time.Now(),
			ProductHint: offer.ProductHint{Brand: "producer", Text: c.title}}
		if err := offers.Put(ctx, o); err != nil {
			t.Fatal(err)
		}
		o.ID = offer.DeterministicID(o.SourceID, o.SourceKey)
		if o.ID != it.ID {
			t.Fatalf("offer and item ids differ for %q", c.title)
		}
		if ok, err := offers.RecordResolution(ctx, o.ID, o.ProductHint.Fingerprint(), offer.Resolution{
			State: offer.ResolutionResolved, ProductID: c.productID, VintageMode: c.mode, At: time.Now(),
		}); err != nil || !ok {
			t.Fatalf("RecordResolution: %v %v", ok, err)
		}
		rows = append(rows, searchRow{ID: it.ID, Title: it.Title, Details: rowDetails(it.Attributes)})
	}
	rows = srv.withProductIDs(ctx, rows)
	key := map[string]searchRow{}
	for i, c := range cases {
		key[c.key] = rows[i]
	}

	if a, b := key["bsc-nv"], key["bsc-2019"]; a.ComparisonKey == "" || a.ComparisonKey != b.ComparisonKey ||
		a.VintageStatus != offer.VintageStatusNonVintage {
		t.Errorf("Special Cuvee NV and 'disgorged 2019' must group: %q / %q (%s)", a.ComparisonKey, b.ComparisonKey, a.VintageStatus)
	}
	if a, b := key["lga-2014"], key["lga-2015"]; a.ComparisonKey == b.ComparisonKey || a.ProductID != b.ProductID {
		t.Errorf("La Grande Annee 2014 and 2015 share a product but must not compare: %q / %q", a.ComparisonKey, b.ComparisonKey)
	}
	if r := key["opus-none"]; r.ComparisonKey != "" || r.VintageStatus != offer.VintageStatusUnknown || r.ProductID != "p-opus-one" {
		t.Errorf("Opus One with no year: key %q status %q, want no key and vintage_unknown", r.ComparisonKey, r.VintageStatus)
	}
	if r := key["opus-2019"]; r.ComparisonKey != "p-opus-one@2019" {
		t.Errorf("Opus One 2019: key %q", r.ComparisonKey)
	}
	if r := key["conflict"]; r.ComparisonKey != "" || r.VintageStatus != offer.VintageStatusConflictNV {
		t.Errorf("NV title on a vintage product: key %q status %q, want none / conflict_nv", r.ComparisonKey, r.VintageStatus)
	}
	if r := key["unknown-nv"]; r.ComparisonKey != "p-hypothetical" || r.VintageStatus != offer.VintageStatusNonVintage {
		t.Errorf("unknown mode with NV: key %q status %q, want the product id / non_vintage", r.ComparisonKey, r.VintageStatus)
	}
	if r := key["est"]; r.ComparisonKey == "p-old-winery@1970" || r.VintageStatus != offer.VintageStatusUnknown {
		t.Errorf("'Est. 1970': key %q status %q, want no key / vintage_unknown", r.ComparisonKey, r.VintageStatus)
	}
	if r := key["opus-bottled"]; r.ComparisonKey != "p-opus-one@2019" {
		t.Errorf("Opus One 2019 (bottled 2021): key %q, want @2019", r.ComparisonKey)
	}
}
