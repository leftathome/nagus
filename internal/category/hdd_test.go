package category

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/connector/ebay"
	"github.com/leftathome/nagus/internal/store"
)

// TestHDDSliceEndToEnd is the automatable form of the vertical-slice proof: it
// drives the real eBay fixture connector -> sanitize -> HDD extractor -> store
// -> hard-filter -> $/TB valuation (against an offline StaticReference) -> score
// -> rank, and asserts the ranked verdicts. This is the same path the CLI runs.
func TestNewHDDPipelineSetsFreshnessWindow(t *testing.T) {
	p := NewHDDPipeline(nil, HDDDeps{Store: store.NewMemoryStore()})
	if p.StaleAfter != EbayContentMaxAge {
		t.Fatalf("HDD pipeline StaleAfter = %v, want %v (eBay 8.1(b) 6h window)", p.StaleAfter, EbayContentMaxAge)
	}
	if EbayContentMaxAge > 6*time.Hour {
		t.Fatalf("EbayContentMaxAge = %v, must be <= 6h per eBay License 8.1(b)", EbayContentMaxAge)
	}
}

func TestHDDSliceEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()

	conn := ebay.NewConnector(ebay.Config{FixturePath: "../connector/ebay/testdata/browse_search.json"})
	ref := StaticReference{CentsPerTB: map[string]int64{"new": 1900, "refurb": 1400, "used": 1150}}
	p := NewHDDPipeline(conn, HDDDeps{Store: st, Reference: ref})

	ing, err := p.Ingest(ctx)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ing.Fetched != 3 || ing.Stored != 3 || len(ing.Skips) != 0 {
		t.Fatalf("ingest: fetched=%d stored=%d skips=%d, want 3/3/0", ing.Fetched, ing.Stored, len(ing.Skips))
	}

	res, err := p.Surface(ctx, store.Query{Category: "hdd"})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if res.Matched != 3 || res.Filtered != 3 {
		t.Fatalf("surface: matched=%d filtered=%d, want 3/3", res.Matched, res.Filtered)
	}

	// Expected ranking (best $/TB first): used 10TB @ $8.95/TB -> great;
	// new 16TB @ $17.50/TB -> good; refurb 8TB @ $16.25/TB -> poor.
	want := []struct {
		condition string
		capTB     string
		verdict   string
	}{
		{"used", "10", "great"},
		{"new", "16", "good"},
		{"refurb", "8", "poor"},
	}
	if len(res.Items) != len(want) {
		t.Fatalf("got %d ranked items, want %d", len(res.Items), len(want))
	}
	for i, w := range want {
		got := res.Items[i]
		if got.Item.Condition != w.condition || got.Item.Attributes["capacity_tb"] != w.capTB || got.Signal.Verdict != w.verdict {
			t.Errorf("rank %d = {cond=%s cap=%s verdict=%s}, want {cond=%s cap=%s verdict=%s}",
				i+1, got.Item.Condition, got.Item.Attributes["capacity_tb"], got.Signal.Verdict,
				w.condition, w.capTB, w.verdict)
		}
	}
	// Ranking must be strictly non-increasing by score.
	for i := 1; i < len(res.Items); i++ {
		if res.Items[i].Score.Value > res.Items[i-1].Score.Value {
			t.Errorf("ranking not descending at %d: %v", i, res.Items)
		}
	}
}

func TestHDDFilterDefaults(t *testing.T) {
	f := HDDFilter(0) // zero -> default floor
	if f.MinAttr["capacity_tb"] != DefaultMinCapacityTB {
		t.Fatalf("HDDFilter(0) capacity floor = %v, want %v", f.MinAttr["capacity_tb"], DefaultMinCapacityTB)
	}
	if !f.RequirePriced || f.Category != "hdd" {
		t.Fatalf("HDDFilter defaults wrong: %+v", f)
	}
}

func TestStaticReferenceMissingTierFallsThrough(t *testing.T) {
	ref := StaticReference{CentsPerTB: map[string]int64{"refurb": 1400}}
	if _, ok, _ := ref.PricePerTB(context.Background(), 10, "new"); ok {
		t.Fatal("expected ok=false for an absent condition tier (lets Valuer fall back)")
	}
	if v, ok, _ := ref.PricePerTB(context.Background(), 10, "refurb"); !ok || v != 1400 {
		t.Fatalf("refurb tier = (%d,%v), want (1400,true)", v, ok)
	}
}
