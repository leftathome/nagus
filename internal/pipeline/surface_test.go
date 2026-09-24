package pipeline

import (
	"context"
	"fmt"
	"strings"
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

// nagus-cb8: the best deals sort LAST in storage order, behind items the
// filter drops. A limit must return the top of the WHOLE ranking.
func limitFixture(t *testing.T) *Surface {
	t.Helper()
	raws := []listing.Raw{
		raw("a-small", "tiny 2TB", 1000, "2"), // filtered
		raw("b-small", "tiny 4TB", 2000, "4"), // filtered
		raw("c-mid", "HGST 10TB", 20000, "10"),
		raw("d-mid", "WD 12TB", 21000, "12"),
		raw("y-good", "Toshiba 14TB", 13000, "14"),
		raw("z-best", "Seagate Exos 18TB", 11000, "18"),
	}
	st := store.NewMemoryStore()
	ing := &Ingester{Connector: fakeConnector{raws: raws}, Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	valuate := func(_ context.Context, it item.Item) (score.DealSignal, error) {
		switch it.ID {
		case "z-best":
			return score.DealSignal{Verdict: "great", Ratio: 0.6, HasReference: true}, nil
		case "y-good":
			return score.DealSignal{Verdict: "good", Ratio: 0.8, HasReference: true}, nil
		}
		return score.DealSignal{Verdict: "market", Ratio: 1.1, HasReference: true}, nil
	}
	filter := score.Filter{Category: "hdd", RequirePriced: true, MinAttr: map[string]float64{"capacity_tb": 8}}
	return &Surface{Store: st, Filter: filter, Valuate: valuate}
}

func TestSurfaceLimitAppliesAfterRanking(t *testing.T) {
	s := limitFixture(t)
	all, err := s.Surface(context.Background(), store.Query{Category: "hdd"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 4 || all.Items[0].Item.ID != "z-best" || all.Items[1].Item.ID != "y-good" {
		t.Fatalf("unlimited ranking = %v", ids(all.Items))
	}
	for _, limit := range []int{1, 2, 3} {
		res, err := s.Surface(context.Background(), store.Query{Category: "hdd", Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := ids(res.Items), ids(all.Items[:limit]); got != want {
			t.Errorf("limit=%d returned %s, want the top of the whole ranking %s", limit, got, want)
		}
		if res.Matched != all.Matched || res.Filtered != all.Filtered {
			t.Errorf("limit=%d: matched/filtered %d/%d, want the whole candidate set %d/%d",
				limit, res.Matched, res.Filtered, all.Matched, all.Filtered)
		}
	}
}

func TestSurfaceCandidateCapIsLogged(t *testing.T) {
	s := limitFixture(t)
	var logs []string
	s.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	s.MaxCandidates = 3
	res, err := s.Surface(context.Background(), store.Query{Category: "hdd", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 3 {
		t.Fatalf("matched %d, want the cap 3", res.Matched)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "candidate cap 3 reached") {
		t.Fatalf("cap not logged: %v", logs)
	}
}

func ids(sc []Scored) string {
	var out []string
	for _, s := range sc {
		out = append(out, s.Item.ID)
	}
	return strings.Join(out, ",")
}
