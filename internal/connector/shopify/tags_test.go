package shopify

import (
	"strings"
	"testing"
)

// The store's tags travel as one comma-joined aspect: the wine extractor
// rejects a listing a store tags "merch" or "pantry" (nagus-tmr).
func TestTagsTravelAsAnAspect(t *testing.T) {
	raws := mustFetch(t, Config{Name: "bottlebarn", FixturePath: "testdata/products_bottlebarn_wine.json", EmitTags: true})
	var found bool
	for _, r := range raws {
		if !strings.HasPrefix(r.Title, "2024 Domaine Tempier Bandol") {
			continue
		}
		found = true
		tags := r.Aspects["tags"]
		if !strings.HasPrefix(tags, "Bandol, Country_France, ") || !strings.Contains(tags, ", Wine Type_Red Wine, ") {
			t.Fatalf("tags aspect %q", tags)
		}
	}
	if !found {
		t.Fatal("fixture product missing")
	}
}

// Without EmitTags (every non-wine source) no listing carries tags: an hdd
// listing's tags would otherwise reach the glovebox gate and the stored
// offer.
func TestTagsOffByDefault(t *testing.T) {
	for _, cfg := range []Config{
		{Name: "serverpartdeals", FixturePath: fixture},
		{Name: "bottlebarn", FixturePath: "testdata/products_bottlebarn_wine.json"},
	} {
		raws := mustFetch(t, cfg)
		if len(raws) == 0 {
			t.Fatalf("%s: no rows", cfg.Name)
		}
		for _, r := range raws {
			if _, ok := r.Aspects["tags"]; ok {
				t.Fatalf("%s row %s carries tags %q", cfg.Name, r.SourceKey, r.Aspects["tags"])
			}
		}
	}
}

func TestJoinTags(t *testing.T) {
	if got := joinTags([]string{" merch ", "", "Pantry", "  "}); got != "merch, Pantry" {
		t.Fatalf("joinTags = %q", got)
	}
	if got := joinTags(nil); got != "" {
		t.Fatalf("joinTags(nil) = %q", got)
	}
}
