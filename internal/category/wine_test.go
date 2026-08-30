package category

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/identity/lwin"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/shipping"
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

// --- channel tagger ---

func TestTagWineChannelStampsDeclarationAndLegalSet(t *testing.T) {
	conn := TagWineChannel(&fakeWineConn{
		id:   "esquin",
		raws: []listing.Raw{rawWine("a", "Wine 2020", "", 1000)},
	}, shipping.Source{Channel: shipping.ChannelRetailer, State: "wa"}, shipping.DefaultRules())

	if conn.SourceID() != "esquin" {
		t.Errorf("tagger must pass through SourceID, got %q", conn.SourceID())
	}
	raws, err := conn.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	asp := raws[0].Aspects
	if asp["wine_channel"] != "retailer" || asp["source_state"] != "WA" {
		t.Fatalf("expected channel+state stamps, got %v", asp)
	}
	// The stamped destination set must agree with the rules table: contains
	// the home state and the out-of-state-permitted CA, not FL.
	set := " " + asp["ship_legal_to"] + " "
	if !strings.Contains(set, " WA ") || !strings.Contains(set, " CA ") {
		t.Errorf("WA retailer's legal set should include WA and CA, got %q", asp["ship_legal_to"])
	}
	if strings.Contains(set, " FL ") {
		t.Errorf("WA retailer must not reach FL by default, got %q", asp["ship_legal_to"])
	}
}

func TestNewWineIngesterRejectsInvalidDeclaration(t *testing.T) {
	deps := WineDeps{Store: store.NewMemoryStore()}
	if _, err := NewWineIngester(&fakeWineConn{id: "x"}, shipping.Source{Channel: "nope", State: "WA"}, deps); err == nil {
		t.Fatalf("an undeclared channel must be a startup error, not a default")
	}
	if _, err := NewWineIngester(&fakeWineConn{id: "x"}, shipping.Source{Channel: shipping.ChannelRetailer, State: "Cascadia"}, deps); err == nil {
		t.Fatalf("an invalid home state must be a startup error")
	}
}

// --- filter ---

func TestWineFilterShape(t *testing.T) {
	f := WineFilter(WineScoreConfig{BudgetCents: 5000, MinScore: 92, ShipTo: "wa"})
	if f.Category != "wine" || !f.RequirePriced || f.MaxPriceCents != 5000 {
		t.Fatalf("unexpected filter base: %+v", f)
	}
	if f.MinAttr["wine_score"] != 92 {
		t.Errorf("MinScore should bound wine_score, got %v", f.MinAttr)
	}
	if f.HasToken["ship_legal_to"] != "WA" {
		t.Errorf("ShipTo should require a normalized destination token, got %v", f.HasToken)
	}

	// Zero config adds no optional bounds.
	f = WineFilter(WineScoreConfig{})
	if len(f.MinAttr) != 0 || len(f.HasToken) != 0 || f.MaxPriceCents != 0 {
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
// tagger (constraint layer stamps legal destinations) -> sanitize -> wine
// extractor (critic parsing + LWIN identity) -> store -> hard-filter
// (score + destination legality) -> hedonic valuation -> score -> rank.
// Two sources share one store: a WA retailer and a CA retailer. The SAME
// corpus is then surfaced for two destinations -- WA (only the WA retailer
// may ship there) and CA (both may: the CA shop in-state, the WA shop under
// CA's out-of-state-retailer allowance) -- which is the whole point of the
// constraint layer over a hardcoded WA rule.
func TestWineSliceEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()

	lwinDB := lwin.NewDB([]lwin.Record{
		{LWIN7: "1101245", Producer: "Leonetti Cellar", Wine: "Cabernet Sauvignon", Region: "Walla Walla", Colour: "red"},
	})
	deps := WineDeps{
		Store: st,
		LWIN:  &lwin.Resolver{DB: lwinDB},
		Score: WineScoreConfig{MinScore: 92, ShipTo: "WA"},
	}

	// WA retailer: a steal, an overpriced bottle, and an unscored one.
	waConn := &fakeWineConn{id: "esquin", raws: []listing.Raw{
		rawWine("steal", "Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", "WS 94 JS 95 JD 96", 4000),
		rawWine("spendy", "Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", "WS 94 JS 95 JD 96", 20000),
		rawWine("unscored", "Mystery Red Blend 2022 750ml", "", 1500),
	}}
	waIng, err := NewWineIngester(waConn, shipping.Source{Channel: shipping.ChannelRetailer, State: "WA"}, deps)
	if err != nil {
		t.Fatalf("NewWineIngester(wa): %v", err)
	}
	res, err := waIng.Ingest(ctx)
	if err != nil {
		t.Fatalf("Ingest(wa): %v", err)
	}
	if res.Fetched != 3 || res.Stored != 3 {
		t.Fatalf("wa ingest: fetched=%d stored=%d, want 3/3", res.Fetched, res.Stored)
	}

	// CA retailer: a great-looking deal that may not ship into WA.
	caConn := &fakeWineConn{id: "ca-wine", raws: []listing.Raw{
		rawWine("ca-steal", "Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", "WS 94 JS 95 JD 96", 4000),
	}}
	caIng, err := NewWineIngester(caConn, shipping.Source{Channel: shipping.ChannelRetailer, State: "CA"}, deps)
	if err != nil {
		t.Fatalf("NewWineIngester(ca): %v", err)
	}
	if _, err := caIng.Ingest(ctx); err != nil {
		t.Fatalf("Ingest(ca): %v", err)
	}

	// Destination WA: only the WA retailer's scored listings survive.
	sr, err := NewWineSurface(deps).Surface(ctx, store.Query{Category: "wine"})
	if err != nil {
		t.Fatalf("Surface(WA): %v", err)
	}
	if len(sr.Items) != 2 {
		for _, sc := range sr.Items {
			t.Logf("surfaced: %s source=%s verdict=%s", sc.Item.Title, sc.Item.SourceID, sc.Signal.Verdict)
		}
		t.Fatalf("WA: expected 2 survivors, got %d (matched=%d)", len(sr.Items), sr.Matched)
	}
	for _, sc := range sr.Items {
		if sc.Item.SourceID != "esquin" {
			t.Errorf("WA: a CA-retailer offer leaked through the legality gate: %s", sc.Item.SourceKey)
		}
	}

	// Ranked order: the steal (negative residual) above the overpriced one.
	best, worst := sr.Items[0], sr.Items[1]
	if best.Item.SourceKey != "steal" || worst.Item.SourceKey != "spendy" {
		t.Fatalf("WA: expected steal ranked above spendy, got %s then %s", best.Item.SourceKey, worst.Item.SourceKey)
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

	// Destination CA over the SAME stored corpus: both retailers may ship
	// there (in-state CA shop; WA shop under CA's out-of-state allowance),
	// so all three scored listings survive.
	caDeps := deps
	caDeps.Score.ShipTo = "CA"
	sr, err = NewWineSurface(caDeps).Surface(ctx, store.Query{Category: "wine"})
	if err != nil {
		t.Fatalf("Surface(CA): %v", err)
	}
	if len(sr.Items) != 3 {
		for _, sc := range sr.Items {
			t.Logf("surfaced: %s source=%s", sc.Item.SourceKey, sc.Item.SourceID)
		}
		t.Fatalf("CA: expected 3 survivors, got %d", len(sr.Items))
	}
	sources := map[string]bool{}
	for _, sc := range sr.Items {
		sources[sc.Item.SourceID] = true
	}
	if !sources["esquin"] || !sources["ca-wine"] {
		t.Errorf("CA: both retailers should surface, got %v", sources)
	}

	// No destination configured: legality is not filtered at all.
	openDeps := deps
	openDeps.Score.ShipTo = ""
	sr, err = NewWineSurface(openDeps).Surface(ctx, store.Query{Category: "wine"})
	if err != nil {
		t.Fatalf("Surface(open): %v", err)
	}
	if len(sr.Items) != 3 {
		t.Fatalf("open: expected 3 survivors without a destination filter, got %d", len(sr.Items))
	}
}

// TestWineSliceRulesOverride pins the data-driven layer end to end: an
// operator override (WA legalizes out-of-state retailer shipping) changes
// what the SAME source may reach, with no code change.
func TestWineSliceRulesOverride(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()

	override := shipping.DefaultRules().Override(shipping.Rules{Destinations: map[string]shipping.Policy{
		"WA": {WineryDirect: true, InStateRetailer: true, OutOfStateRetailer: true},
	}})
	deps := WineDeps{
		Store: st,
		Ship:  &override,
		Score: WineScoreConfig{ShipTo: "WA", MinScoreCount: 1},
	}

	caConn := &fakeWineConn{id: "ca-wine", raws: []listing.Raw{
		rawWine("ca-deal", "Fine Cabernet Sauvignon 2019 750ml", "WS 94", 3000),
	}}
	ing, err := NewWineIngester(caConn, shipping.Source{Channel: shipping.ChannelRetailer, State: "CA"}, deps)
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
		t.Fatalf("under the override, the CA shop should reach WA; got %d survivors", len(sr.Items))
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
	ing, err := NewWineIngester(conn, shipping.Source{Channel: shipping.ChannelRetailer, State: "WA"}, deps)
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
