package ebay

import (
	"context"
	"testing"
	"time"
)

// The fixture follows the Browse API item_summary/search ItemSummary shape
// (buyingOptions, itemEndDate, itemCreationDate as documented by eBay): an
// auction with Buy It Now and an end date, a fixed-price listing with none, a
// fixed-price listing that nevertheless carries an itemEndDate, and an auction
// whose end date does not parse. Usernames are placeholders (never emitted).
func TestFetchDecodesAuctionEndOnly(t *testing.T) {
	c := NewConnector(Config{FixturePath: "testdata/browse_search_buying_options.json"})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(raws) != 4 {
		t.Fatalf("got %d raws, want 4", len(raws))
	}
	byKey := map[string]int{}
	for i, r := range raws {
		byKey[r.SourceKey] = i
	}
	want := map[string]struct {
		ends    time.Time
		options string
	}{
		// AUCTION + a parseable itemEndDate: the source states the end.
		"v1|110599999911|0": {time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC), "AUCTION,FIXED_PRICE"},
		// fixed price, no end date: none.
		"v1|110599999912|0": {time.Time{}, "FIXED_PRICE,BEST_OFFER"},
		// fixed price WITH an itemEndDate: not a committed end, ignored.
		"v1|110599999913|0": {time.Time{}, "FIXED_PRICE"},
		// auction whose end date does not parse: none, rather than a guess.
		"v1|110599999914|0": {time.Time{}, "AUCTION"},
	}
	for key, w := range want {
		i, ok := byKey[key]
		if !ok {
			t.Fatalf("missing %s", key)
		}
		r := raws[i]
		if !r.EndsAt.Equal(w.ends) {
			t.Errorf("%s: EndsAt = %v, want %v", key, r.EndsAt, w.ends)
		}
		if r.Aspects["buying_options"] != w.options {
			t.Errorf("%s: buying_options = %q, want %q", key, r.Aspects["buying_options"], w.options)
		}
	}
}

// The original fixture has neither field: nothing changes for it.
func TestFetchWithoutBuyingOptionsStatesNoEnd(t *testing.T) {
	c := NewConnector(Config{FixturePath: "testdata/browse_search.json"})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, r := range raws {
		if !r.EndsAt.IsZero() {
			t.Errorf("%s: EndsAt = %v, want zero", r.SourceKey, r.EndsAt)
		}
		if _, ok := r.Aspects["buying_options"]; ok {
			t.Errorf("%s: buying_options aspect set with no buyingOptions in the response", r.SourceKey)
		}
	}
}
