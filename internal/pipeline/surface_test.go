package pipeline

import (
	"context"
	"testing"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
)

func TestSurfaceUnitFilterBeforeEnrichAndRank(t *testing.T) {
	raws := []listing.Raw{
		raw("big", "Seagate Exos 16TB", 12000, "16"), // passes filter (cap>=8)
		raw("small", "tiny 4TB", 4000, "4"),          // filtered out (cap<8)
		raw("mid", "HGST 10TB", 20000, "10"),         // passes filter
	}
	st := store.NewMemoryStore()
	ing := &Ingester{Connector: fakeConnector{raws: raws}, Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Hard-filter requires capacity >= 8 TB and a known price.
	filter := score.Filter{Category: "hdd", RequirePriced: true, MinAttr: map[string]float64{"capacity_tb": 8}}

	// Valuate must be called ONLY on hard-filter survivors (filter-before-enrich).
	var valuatedIDs []string
	valuate := func(_ context.Context, it item.Item) (score.DealSignal, error) {
		valuatedIDs = append(valuatedIDs, it.ID)
		// Make "big" the better deal so ranking is deterministic and checkable.
		if it.ID == "big" {
			return score.DealSignal{Verdict: "great", Ratio: 0.7, HasReference: true}, nil
		}
		return score.DealSignal{Verdict: "market", Ratio: 1.05, HasReference: true}, nil
	}

	s := &Surface{Store: st, Filter: filter, Valuate: valuate}
	res, err := s.Surface(context.Background(), store.Query{Category: "hdd"})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if res.Matched != 3 {
		t.Fatalf("Matched=%d, want 3 stored hdd items", res.Matched)
	}
	if res.Filtered != 2 || len(res.Items) != 2 {
		t.Fatalf("Filtered=%d len=%d, want 2 (small dropped by capacity)", res.Filtered, len(res.Items))
	}
	for _, id := range valuatedIDs {
		if id == "small" {
			t.Fatal("filter-before-enrich violated: valuation ran on a filtered-out item")
		}
	}
	if len(valuatedIDs) != 2 {
		t.Fatalf("expected 2 valuations (survivors only), got %d: %v", len(valuatedIDs), valuatedIDs)
	}
	if res.Items[0].Item.ID != "big" {
		t.Fatalf("expected best-first ranking to put 'big' first, got %q", res.Items[0].Item.ID)
	}
	if res.Items[0].Score.Value <= res.Items[1].Score.Value {
		t.Fatalf("ranking not descending by score: %v", res.Items)
	}
}

func TestSurfaceUnitNilValuateDegrades(t *testing.T) {
	raws := []listing.Raw{raw("a", "Seagate 16TB", 12000, "16")}
	st := store.NewMemoryStore()
	ing := &Ingester{Connector: fakeConnector{raws: raws}, Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// No Filter (zero value passes), no Valuate: item still surfaces, unscored.
	s := &Surface{Store: st}
	res, err := s.Surface(context.Background(), store.Query{})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Signal.Verdict != "unknown-no-reference" {
		t.Fatalf("expected 1 unscored item, got %+v", res.Items)
	}
}
