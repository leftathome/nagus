package main

import (
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/category"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
)

func TestPriceHistoryWindowFromEnv(t *testing.T) {
	cases := []struct {
		val     string
		want    time.Duration
		wantErr bool
	}{
		{"", offer.DefaultPriceHistoryWindow, false},
		{"720h", 720 * time.Hour, false},
		{" 8760h ", 8760 * time.Hour, false},
		{"0", 0, false},
		{"-1h", 0, true},
		{"365d", 0, true}, // Go durations have no days: refuse rather than guess
		{"banana", 0, true},
	}
	for _, c := range cases {
		t.Setenv(envPriceHistoryRetention, c.val)
		got, err := priceHistoryWindowFromEnv()
		if (err != nil) != c.wantErr || (!c.wantErr && got != c.want) {
			t.Errorf("%q: got %v, %v; want %v (error %v)", c.val, got, err, c.want, c.wantErr)
		}
	}
	if offer.DefaultPriceHistoryWindow != 365*24*time.Hour {
		t.Fatalf("default window = %v, want 365 days", offer.DefaultPriceHistoryWindow)
	}
}

// A retain-full source's price history is bounded by the configured window;
// eBay's follows its 6h content window (License 8.1(b)) whatever is
// configured, because Purge ignores HistoryWindow (offer.HistoryCutoff).
func TestBuildIngesterAppliesPriceHistoryWindow(t *testing.T) {
	o := categoryOpts{logf: t.Logf, offers: offer.NewMemoryStore(), priceHistoryWindow: 48 * time.Hour}

	shop, err := buildIngester(SourceConfig{
		Name: "spd", Type: "shopify", BaseURL: "https://example.test", IntervalMinutes: 60,
		Fixture: "../../internal/connector/shopify/testdata/products.json",
	}, CategoryConfig{}, nil, o)
	if err != nil {
		t.Fatalf("shopify offer-only source: %v", err)
	}
	if r := shop.OfferRetention; r.Policy != offer.RetainFull || r.HistoryWindow != 48*time.Hour {
		t.Fatalf("shopify retention = %+v, want retain-full with a 48h history window", r)
	}

	eb, err := buildIngester(SourceConfig{
		Name: "ebay", Type: "ebay", IntervalMinutes: 30,
		Fixture: "../../internal/connector/ebay/testdata/browse_search.json",
	}, CategoryConfig{}, nil, o)
	if err != nil {
		t.Fatalf("ebay offer-only source: %v", err)
	}
	r := eb.OfferRetention
	if r.Policy != offer.Purge || r.Window != category.EbayContentMaxAge {
		t.Fatalf("ebay retention = %+v, want purge on the content window", r)
	}
	now := time.Unix(1_750_000_000, 0).UTC()
	if cut, ok := r.HistoryCutoff(now); !ok || !cut.Equal(now.Add(-category.EbayContentMaxAge)) {
		t.Fatalf("ebay history cutoff = %v %v, want the 6h content window", cut, ok)
	}

	// The read path learns the same bound, so it never serves an eBay point
	// older than the content window; a retain-full source has none.
	pol := buildSourcePolicies([]SourceConfig{{Name: "ebay", Type: "ebay", IntervalMinutes: 30}, {Name: "spd", Type: "shopify", IntervalMinutes: 60}},
		[]*pipeline.Ingester{eb, shop})
	if got := pol[eb.SourceID()].historyMaxAge; got != category.EbayContentMaxAge {
		t.Fatalf("ebay historyMaxAge = %v, want %v", got, category.EbayContentMaxAge)
	}
	if got := pol[shop.SourceID()].historyMaxAge; got != 0 {
		t.Fatalf("shopify historyMaxAge = %v, want 0", got)
	}
}
