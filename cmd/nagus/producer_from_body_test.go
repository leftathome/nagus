package main

import (
	"context"
	"testing"

	"github.com/leftathome/nagus/internal/store"
)

// producerFromBody must reach the ingester: a per-source flag added to the
// config and to the category, but not passed between them, silently does
// nothing (the same wiring gap once stored every retailer listing without the
// flag it was declared with, 2026-09-22). Real Bottle Barn listings, whose
// Shopify vendor is the useless "WINE" and whose descriptions open with
// "Producer: <name>".
func TestProducerFromBodyReachesTheItems(t *testing.T) {
	for _, tc := range []struct {
		name   string
		enable bool
	}{{"enabled", true}, {"disabled", false}} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemoryStore()
			src := SourceConfig{Name: "bottlebarn", Category: "wine", Type: "shopify",
				Fixture:     "../../internal/connector/shopify/testdata/products_bottlebarn_wine.json",
				WineChannel: "retailer", Origin: "US-CA", ProducerFromBody: tc.enable}
			ing, err := buildIngester(src, CategoryConfig{WineShipTo: "US-WA"}, st, categoryOpts{logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ing.Ingest(context.Background()); err != nil {
				t.Fatal(err)
			}
			items, err := st.Search(context.Background(), store.Query{Category: "wine", Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			if len(items) == 0 {
				t.Fatal("fixture yielded no wine items")
			}
			for _, it := range items {
				p := it.Attributes["producer"]
				if tc.enable && p == "" {
					t.Fatalf("%q: producerFromBody set but no producer reached the item", it.Title)
				}
				if !tc.enable && p != "" {
					t.Fatalf("%q: a retailer's listing got producer %q without the opt-in", it.Title, p)
				}
			}
		})
	}
}
