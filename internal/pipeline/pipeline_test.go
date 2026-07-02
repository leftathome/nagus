package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
)

// fakeConnector emits a fixed batch (or an error).
type fakeConnector struct {
	raws []listing.Raw
	err  error
}

func (f fakeConnector) SourceID() string { return "fake" }
func (f fakeConnector) Fetch(context.Context) ([]listing.Raw, error) {
	return f.raws, f.err
}

// fakeExtractor turns a Sanitized into an hdd item, copying a capacity aspect
// into Attributes. It errors when SourceKey is "bad" to exercise the skip path.
type fakeExtractor struct{}

func (fakeExtractor) Category() string { return "hdd" }
func (fakeExtractor) Extract(_ context.Context, s listing.Sanitized) (item.Item, error) {
	if s.SourceKey == "bad" {
		return item.Item{}, errors.New("unextractable")
	}
	attrs := map[string]string{}
	if c, ok := s.Aspects["capacity_tb"]; ok {
		attrs["capacity_tb"] = c
	}
	return item.Item{
		ID: s.SourceKey, Category: "hdd", Class: item.ClassDurable,
		Title: s.Title, PriceCents: s.PriceCents, Currency: s.Currency,
		Condition: s.ConditionRaw, SourceID: s.SourceID, SourceKey: s.SourceKey,
		SourceURL: s.SourceURL, SeenAt: s.SeenAt, Attributes: attrs,
	}, nil
}

func raw(key, title string, cents int64, capTB string) listing.Raw {
	return listing.Raw{
		SourceID: "fake", SourceKey: key, Title: title, PriceCents: cents,
		Currency: "USD", ConditionRaw: "refurb",
		Aspects: map[string]string{"capacity_tb": capTB}, SeenAt: time.Unix(1000, 0),
	}
}

func newPipeline(t *testing.T, raws []listing.Raw) (*Pipeline, store.Store) {
	t.Helper()
	st := store.NewMemoryStore()
	p := &Pipeline{
		Connector: fakeConnector{raws: raws},
		Sanitizer: sanitize.Passthrough{},
		Extractor: fakeExtractor{},
		Store:     st,
	}
	return p, st
}

func TestIngestStoresAndSkips(t *testing.T) {
	raws := []listing.Raw{
		raw("a", "Seagate 16TB", 12000, "16"),
		raw("bad", "broken listing", 9999, "8"),
		raw("b", "WD 8TB", 8000, "8"),
	}
	p, st := newPipeline(t, raws)
	res, err := p.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest error: %v", err)
	}
	if res.Fetched != 3 || res.Stored != 2 {
		t.Fatalf("Fetched=%d Stored=%d, want 3/2", res.Fetched, res.Stored)
	}
	if len(res.Skips) != 1 || res.Skips[0].Stage != "extract" || res.Skips[0].SourceKey != "bad" {
		t.Fatalf("expected one extract skip for 'bad', got %+v", res.Skips)
	}
	if _, ok, _ := st.Get(context.Background(), "a"); !ok {
		t.Fatal("item 'a' not stored")
	}
	if _, ok, _ := st.Get(context.Background(), "bad"); ok {
		t.Fatal("item 'bad' should not be stored")
	}
}

func TestIngestConnectorErrorAborts(t *testing.T) {
	st := store.NewMemoryStore()
	p := &Pipeline{
		Connector: fakeConnector{err: errors.New("network down")},
		Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st,
	}
	if _, err := p.Ingest(context.Background()); err == nil {
		t.Fatal("expected Ingest to propagate the connector Fetch error")
	}
}

func TestSurfaceFilterBeforeEnrichAndRank(t *testing.T) {
	raws := []listing.Raw{
		raw("big", "Seagate Exos 16TB", 12000, "16"), // passes filter (cap>=8)
		raw("small", "tiny 4TB", 4000, "4"),          // filtered out (cap<8)
		raw("mid", "HGST 10TB", 20000, "10"),         // passes filter
	}
	p, _ := newPipeline(t, raws)
	if _, err := p.Ingest(context.Background()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Hard-filter requires capacity >= 8 TB and a known price.
	p.Filter = score.Filter{Category: "hdd", RequirePriced: true, MinAttr: map[string]float64{"capacity_tb": 8}}

	// Valuate must be called ONLY on hard-filter survivors (filter-before-enrich).
	var valuatedIDs []string
	p.Valuate = func(_ context.Context, it item.Item) (score.DealSignal, error) {
		valuatedIDs = append(valuatedIDs, it.ID)
		// Make "big" the better deal so ranking is deterministic and checkable.
		if it.ID == "big" {
			return score.DealSignal{Verdict: "great", Ratio: 0.7, HasReference: true}, nil
		}
		return score.DealSignal{Verdict: "market", Ratio: 1.05, HasReference: true}, nil
	}

	res, err := p.Surface(context.Background(), store.Query{Category: "hdd"})
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

func TestSurfaceNilValuateDegrades(t *testing.T) {
	raws := []listing.Raw{raw("a", "Seagate 16TB", 12000, "16")}
	p, _ := newPipeline(t, raws)
	if _, err := p.Ingest(context.Background()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// No Filter (zero value passes), no Valuate: item still surfaces, unscored.
	res, err := p.Surface(context.Background(), store.Query{})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Signal.Verdict != "unknown-no-reference" {
		t.Fatalf("expected 1 unscored item, got %+v", res.Items)
	}
}
