package main

import (
	"context"
	"testing"
)

// nagus-tmr review: only a wine shopify source emits the tags aspect; an
// hdd source's listings carry none (they would reach the glovebox gate and
// every stored offer).
func TestShopifyTagsOnlyForWineSources(t *testing.T) {
	for _, tc := range []struct {
		category string
		want     bool
	}{{"wine", true}, {"hdd", false}, {"", false}} {
		conn, err := buildShopifyConnector(SourceConfig{
			Name: "s", Type: "shopify", Category: tc.category,
			Fixture: "../../internal/connector/shopify/testdata/products_bottlebarn_wine.json",
		}, categoryOpts{})
		if err != nil {
			t.Fatal(err)
		}
		raws, err := conn.Fetch(context.Background())
		if err != nil || len(raws) == 0 {
			t.Fatalf("%q: err=%v rows=%d", tc.category, err, len(raws))
		}
		for _, r := range raws {
			if _, has := r.Aspects["tags"]; has != tc.want {
				t.Fatalf("category %q: row %s has tags=%v, want %v", tc.category, r.SourceKey, has, tc.want)
			}
		}
	}
}
