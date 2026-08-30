package category

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/identity/lwin"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/store"
)

// fakeWineConn is a stub connector emitting fixed raw listings (one wine
// source; the channel tagger wraps it in the bundle).
type fakeWineConn struct {
	id   string
	raws []listing.Raw
}

func (f *fakeWineConn) SourceID() string { return f.id }

// Fetch stamps the connector's own SourceID onto every Raw, as the
// listing.Connector contract requires of real connectors.
func (f *fakeWineConn) Fetch(context.Context) ([]listing.Raw, error) {
	out := make([]listing.Raw, len(f.raws))
	copy(out, f.raws)
	for i := range out {
		out[i].SourceID = f.id
	}
	return out, nil
}

func rawWine(key, title, body string, priceCents int64) listing.Raw {
	return listing.Raw{
		SourceKey:  key,
		SourceURL:  "https://example.com/" + key,
		Title:      title,
		Body:       body,
		PriceCents: priceCents,
		Currency:   "USD",
		SeenAt:     time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC),
	}
}

// --- channel semantics ---

func TestWineChannelShipLegalWA(t *testing.T) {
	cases := []struct {
		ch   WineChannel
		want bool
	}{
		{WineChannelWARetailer, true},
		{WineChannelWineryDirect, true},
		{WineChannelOutOfStateRetailer, false},
		{WineChannel("mystery"), false}, // fail closed
		{WineChannel(""), false},
	}
	for _, c := range cases {
		if got := c.ch.ShipLegalWA(); got != c.want {
			t.Errorf("ShipLegalWA(%q) = %v, want %v", c.ch, got, c.want)
		}
	}
}

func TestTagWineChannelStampsAspects(t *testing.T) {
	conn := TagWineChannel(&fakeWineConn{
		id:   "esquin",
		raws: []listing.Raw{rawWine("a", "Wine 2020", "", 1000)},
	}, WineChannelWARetailer)

	if conn.SourceID() != "esquin" {
		t.Errorf("tagger must pass through SourceID, got %q", conn.SourceID())
	}
	raws, err := conn.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if raws[0].Aspects["wine_channel"] != "wa_retailer" || raws[0].Aspects["ship_legal_wa"] != "true" {
		t.Fatalf("expected channel stamps, got %v", raws[0].Aspects)
	}
}

func TestNewWineIngesterRejectsUnknownChannel(t *testing.T) {
	_, err := NewWineIngester(&fakeWineConn{id: "x"}, WineChannel("nope"), WineDeps{Store: store.NewMemoryStore()})
	if err == nil {
		t.Fatalf("an undeclared channel must be a startup error, not a default")
	}
}

// --- filter ---

func TestWineFilterShape(t *testing.T) {
	f := WineFilter(WineScoreConfig{BudgetCents: 5000, MinScore: 92, RequireShipLegalWA: true})
	if f.Category != "wine" || !f.RequirePriced || f.MaxPriceCents != 5000 {
		t.Fatalf("unexpected filter base: %+v", f)
	}
	if f.MinAttr["wine_score"] != 92 {
		t.Errorf("MinScore should bound wine_score, got %v", f.MinAttr)
	}
	if f.EqAttr["ship_legal_wa"] != "true" {
		t.Errorf("legality gate missing: %v", f.EqAttr)
	}

	// Zero config adds no optional bounds.
	f = WineFilter(WineScoreConfig{})
	if len(f.MinAttr) != 0 || len(f.EqAttr) != 0 || f.MaxPriceCents != 0 {
		t.Errorf("zero config should add no bounds: %+v", f)
	}
}

// --- 750ml price normalization ---

func TestPrice750Equivalent(t *testing.T) {
	it := item.Item{PriceCents: 10000, Attributes: map[string]string{"bottle_ml": "1500"}}
	if got := price750Equivalent(it); got != 5000 {
		t.Errorf("magnum price should halve, got %d", got)
	}
	it.Attributes["bottle_ml"] = "750"
	if got := price750Equivalent(it); got != 10000 {
		t.Errorf("standard bottle unchanged, got %d", got)
	}
	it.Attributes = nil
	if got := price750Equivalent(it); got != 10000 {
		t.Errorf("missing size treated as standard, got %d", got)
	}
}

// --- end to end ---

// TestWineSliceEndToEnd drives the whole wine path: connector -> channel
// tagger -> sanitize -> wine extractor (critic parsing + LWIN identity) ->
// store -> hard-filter (score + WA legality) -> hedonic valuation -> score ->
// rank. Two sources: an in-state retailer (legal) and an out-of-state
// retailer (illegal-to-ship), sharing one store.
func TestWineSliceEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()

	lwinDB := lwin.NewDB([]lwin.Record{
		{LWIN7: "1101245", Producer: "Leonetti Cellar", Wine: "Cabernet Sauvignon", Region: "Walla Walla", Colour: "red"},
	})
	deps := WineDeps{
		Store: st,
		LWIN:  &lwin.Resolver{DB: lwinDB},
		Score: WineScoreConfig{MinScore: 92, RequireShipLegalWA: true},
	}

	// In-state retailer: a steal, an overpriced bottle, and an unscored one.
	waConn := &fakeWineConn{id: "esquin", raws: []listing.Raw{
		rawWine("steal", "Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", "WS 94 JS 95 JD 96", 4000),
		rawWine("spendy", "Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", "WS 94 JS 95 JD 96", 20000),
		rawWine("unscored", "Mystery Red Blend 2022 750ml", "", 1500),
	}}
	waIng, err := NewWineIngester(waConn, WineChannelWARetailer, deps)
	if err != nil {
		t.Fatalf("NewWineIngester: %v", err)
	}
	res, err := waIng.Ingest(ctx)
	if err != nil {
		t.Fatalf("Ingest(wa): %v", err)
	}
	if res.Fetched != 3 || res.Stored != 3 {
		t.Fatalf("wa ingest: fetched=%d stored=%d, want 3/3", res.Fetched, res.Stored)
	}

	// Out-of-state retailer: a great-looking deal that is illegal to ship.
	oosConn := &fakeWineConn{id: "oos-wine", raws: []listing.Raw{
		rawWine("oos-steal", "Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", "WS 94 JS 95 JD 96", 4000),
	}}
	oosIng, err := NewWineIngester(oosConn, WineChannelOutOfStateRetailer, deps)
	if err != nil {
		t.Fatalf("NewWineIngester: %v", err)
	}
	if _, err := oosIng.Ingest(ctx); err != nil {
		t.Fatalf("Ingest(oos): %v", err)
	}

	sr, err := NewWineSurface(deps).Surface(ctx, store.Query{Category: "wine"})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}

	// Survivors: only the two scored, WA-legal listings. The unscored one
	// fails MinScore; the out-of-state steal fails the legality gate.
	if len(sr.Items) != 2 {
		for _, sc := range sr.Items {
			t.Logf("surfaced: %s source=%s verdict=%s", sc.Item.Title, sc.Item.SourceID, sc.Signal.Verdict)
		}
		t.Fatalf("expected 2 survivors, got %d (matched=%d)", len(sr.Items), sr.Matched)
	}
	for _, sc := range sr.Items {
		if sc.Item.SourceID != "esquin" {
			t.Errorf("an out-of-state offer leaked through the legality gate: %+v", sc.Item)
		}
	}

	// Ranked order: the steal (negative residual) above the overpriced one.
	best, worst := sr.Items[0], sr.Items[1]
	if best.Item.SourceKey != "steal" || worst.Item.SourceKey != "spendy" {
		t.Fatalf("expected steal ranked above spendy, got %s then %s", best.Item.SourceKey, worst.Item.SourceKey)
	}
	if best.Signal.Verdict != "great" {
		t.Errorf("a half-price 95-pointer should be great, got %q (ratio %.2f)", best.Signal.Verdict, best.Signal.Ratio)
	}
	if worst.Signal.Verdict != "poor" {
		t.Errorf("a double-price 95-pointer should be poor, got %q (ratio %.2f)", worst.Signal.Verdict, worst.Signal.Ratio)
	}

	// Identity: the LWIN resolver stamped the canonical id on the survivors.
	if best.Item.CanonicalID != "11012452019" {
		t.Errorf("expected LWIN-11 canonical id, got %q", best.Item.CanonicalID)
	}
}

// TestWineSurfaceMinScoreCountGate pins the minimum-3 rule: with only two
// critic scores the valuation must refuse to flag value even when the filter
// passes the item.
func TestWineSurfaceMinScoreCountGate(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	deps := WineDeps{Store: st, Score: WineScoreConfig{}}

	conn := &fakeWineConn{id: "shop", raws: []listing.Raw{
		rawWine("two-scores", "Fine Cabernet Sauvignon 2019", "WS 95 JS 96", 2000),
	}}
	ing, err := NewWineIngester(conn, WineChannelWARetailer, deps)
	if err != nil {
		t.Fatalf("NewWineIngester: %v", err)
	}
	if _, err := ing.Ingest(ctx); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	sr, err := NewWineSurface(deps).Surface(ctx, store.Query{Category: "wine"})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if len(sr.Items) != 1 {
		t.Fatalf("expected 1 survivor, got %d", len(sr.Items))
	}
	if sr.Items[0].Signal.Verdict != "unknown-no-reference" {
		t.Fatalf("two scores must not flag value (min-3 rule), got %q", sr.Items[0].Signal.Verdict)
	}

	// Lowering the bar is an explicit operator decision.
	deps.Score.MinScoreCount = 1
	sr, err = NewWineSurface(deps).Surface(ctx, store.Query{Category: "wine"})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if sr.Items[0].Signal.Verdict == "unknown-no-reference" {
		t.Fatalf("MinScoreCount=1 should allow flagging, got %q", sr.Items[0].Signal.Verdict)
	}
}
