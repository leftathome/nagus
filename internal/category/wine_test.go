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

func rawWine(key, title, body string, priceCents int64, currency string) listing.Raw {
	return listing.Raw{
		SourceKey:  key,
		SourceURL:  "https://example.com/" + key,
		Title:      title,
		Body:       body,
		PriceCents: priceCents,
		Currency:   currency,
		SeenAt:     time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC),
	}
}

func mustSource(t *testing.T, channel, origin string) shipping.Source {
	t.Helper()
	s, err := shipping.NewSource(channel, origin)
	if err != nil {
		t.Fatalf("shipping.NewSource(%q,%q): %v", channel, origin, err)
	}
	return s
}

// --- channel tagger ---

func TestTagWineChannelStampsDeclarationAndLegalSet(t *testing.T) {
	conn := TagWineChannel(&fakeWineConn{
		id:   "esquin",
		raws: []listing.Raw{rawWine("a", "Wine 2020", "", 1000, "USD")},
	}, mustSource(t, "retailer", "us-wa"), shipping.DefaultRules())

	if conn.SourceID() != "esquin" {
		t.Errorf("tagger must pass through SourceID, got %q", conn.SourceID())
	}
	raws, err := conn.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	asp := raws[0].Aspects
	if asp["wine_channel"] != "retailer" || asp["source_origin"] != "US-WA" {
		t.Fatalf("expected channel+origin stamps, got %v", asp)
	}
	// The stamped destination set must agree with the rules table: the home
	// state and the inbound-permitting CA, but not FL.
	set := " " + asp["ship_legal_to"] + " "
	if !strings.Contains(set, " US-WA ") || !strings.Contains(set, " US-CA ") {
		t.Errorf("WA retailer's legal set should include US-WA and US-CA, got %q", asp["ship_legal_to"])
	}
	if strings.Contains(set, " US-FL ") {
		t.Errorf("WA retailer must not reach US-FL by default, got %q", asp["ship_legal_to"])
	}
}

func TestTagWineChannelStampsInternationalSource(t *testing.T) {
	conn := TagWineChannel(&fakeWineConn{
		id:   "domaine",
		raws: []listing.Raw{rawWine("a", "Bourgogne 2020", "", 3000, "EUR")},
	}, mustSource(t, "producer", "FR"), shipping.DefaultRules())

	raws, err := conn.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	asp := raws[0].Aspects
	if asp["source_origin"] != "FR" {
		t.Errorf("country-level origin should stamp as FR, got %q", asp["source_origin"])
	}
	set := " " + asp["ship_legal_to"] + " "
	for _, want := range []string{" FR ", " ES ", " IT "} {
		if !strings.Contains(set, want) {
			t.Errorf("an FR producer should reach%s got %q", want, asp["ship_legal_to"])
		}
	}
	if strings.Contains(set, " US-") || strings.Contains(set, " CA-") {
		t.Errorf("an FR producer must not reach US/CA destinations by default, got %q", asp["ship_legal_to"])
	}
}

func TestNewWineIngesterRejectsInvalidDeclaration(t *testing.T) {
	deps := WineDeps{Store: store.NewMemoryStore()}
	bad := []shipping.Source{
		{Channel: "nope", Origin: shipping.Jurisdiction{Country: "US", Subdivision: "WA"}},
		{Channel: shipping.ChannelRetailer}, // no origin
	}
	for _, src := range bad {
		if _, err := NewWineIngester(&fakeWineConn{id: "x"}, src, deps); err == nil {
			t.Errorf("%+v must be a startup error, not a default", src)
		}
	}
}

// --- filter ---

func TestWineFilterShape(t *testing.T) {
	f := WineFilter(WineScoreConfig{BudgetCents: 5000, MinScore: 92, ShipTo: "us-wa"})
	if f.Category != "wine" || !f.RequirePriced || f.MaxPriceCents != 5000 {
		t.Fatalf("unexpected filter base: %+v", f)
	}
	if f.MinAttr["wine_score"] != 92 {
		t.Errorf("MinScore should bound wine_score, got %v", f.MinAttr)
	}
	if f.HasToken["ship_legal_to"] != "US-WA" {
		t.Errorf("ShipTo should require a normalized destination token, got %v", f.HasToken)
	}

	// A country-level destination works the same way.
	f = WineFilter(WineScoreConfig{ShipTo: "fr"})
	if f.HasToken["ship_legal_to"] != "FR" {
		t.Errorf("country-level ShipTo should normalize, got %v", f.HasToken)
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
// Two US retailers share one store, and the SAME corpus is surfaced for two
// destinations -- WA (only the in-state shop may ship there) and CA (both
// may) -- which is the point of a constraint layer over a hardcoded rule.
func TestWineSliceEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()

	lwinDB := lwin.NewDB([]lwin.Record{
		{LWIN7: "1101245", Producer: "Leonetti Cellar", Wine: "Cabernet Sauvignon", Region: "Walla Walla", Colour: "red"},
	})
	deps := WineDeps{
		Store: st,
		LWIN:  &lwin.Resolver{DB: lwinDB},
		Score: WineScoreConfig{MinScore: 92, ShipTo: "US-WA"},
	}

	// WA retailer: a steal, an overpriced bottle, and an unscored one.
	waConn := &fakeWineConn{id: "esquin", raws: []listing.Raw{
		rawWine("steal", "Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", "WS 94 JS 95 JD 96", 4000, "USD"),
		rawWine("spendy", "Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", "WS 94 JS 95 JD 96", 20000, "USD"),
		rawWine("unscored", "Mystery Red Blend 2022 750ml", "", 1500, "USD"),
	}}
	waIng, err := NewWineIngester(waConn, mustSource(t, "retailer", "US-WA"), deps)
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
		rawWine("ca-steal", "Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", "WS 94 JS 95 JD 96", 4000, "USD"),
	}}
	caIng, err := NewWineIngester(caConn, mustSource(t, "retailer", "US-CA"), deps)
	if err != nil {
		t.Fatalf("NewWineIngester(ca): %v", err)
	}
	if _, err := caIng.Ingest(ctx); err != nil {
		t.Fatalf("Ingest(ca): %v", err)
	}

	// Destination US-WA: only the WA retailer's scored listings survive.
	sr, err := NewWineSurface(deps).Surface(ctx, store.Query{Category: "wine"})
	if err != nil {
		t.Fatalf("Surface(US-WA): %v", err)
	}
	if len(sr.Items) != 2 {
		for _, sc := range sr.Items {
			t.Logf("surfaced: %s source=%s verdict=%s", sc.Item.Title, sc.Item.SourceID, sc.Signal.Verdict)
		}
		t.Fatalf("US-WA: expected 2 survivors, got %d (matched=%d)", len(sr.Items), sr.Matched)
	}
	for _, sc := range sr.Items {
		if sc.Item.SourceID != "esquin" {
			t.Errorf("US-WA: a CA-retailer offer leaked through the legality gate: %s", sc.Item.SourceKey)
		}
	}

	// Ranked order: the steal (negative residual) above the overpriced one.
	best, worst := sr.Items[0], sr.Items[1]
	if best.Item.SourceKey != "steal" || worst.Item.SourceKey != "spendy" {
		t.Fatalf("US-WA: expected steal ranked above spendy, got %s then %s", best.Item.SourceKey, worst.Item.SourceKey)
	}
	if best.Signal.Verdict != "great" {
		t.Errorf("a half-price 95-pointer should be great, got %q (ratio %.2f)", best.Signal.Verdict, best.Signal.Ratio)
	}
	if worst.Signal.Verdict != "poor" {
		t.Errorf("a double-price 95-pointer should be poor, got %q (ratio %.2f)", worst.Signal.Verdict, worst.Signal.Ratio)
	}
	if best.Item.CanonicalID != "11012452019" {
		t.Errorf("expected LWIN-11 canonical id, got %q", best.Item.CanonicalID)
	}

	// Destination US-CA over the SAME stored corpus: both retailers may ship
	// there (the CA shop in-state, the WA shop under CA's inbound allowance).
	caDeps := deps
	caDeps.Score.ShipTo = "US-CA"
	sr, err = NewWineSurface(caDeps).Surface(ctx, store.Query{Category: "wine"})
	if err != nil {
		t.Fatalf("Surface(US-CA): %v", err)
	}
	if len(sr.Items) != 3 {
		t.Fatalf("US-CA: expected 3 survivors, got %d", len(sr.Items))
	}
	sources := map[string]bool{}
	for _, sc := range sr.Items {
		sources[sc.Item.SourceID] = true
	}
	if !sources["esquin"] || !sources["ca-wine"] {
		t.Errorf("US-CA: both retailers should surface, got %v", sources)
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

// TestWineSliceInternational is the point of the generalization: one corpus
// of European and US sources, surfaced for a Spanish buyer, a US buyer, and a
// gift recipient in Canada -- each seeing only what may legally reach them.
func TestWineSliceInternational(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()

	// A USD-fit model plus a EUR rate, so European prices are placed rather
	// than mispriced or dropped.
	deps := WineDeps{
		Store: st,
		Rates: map[string]float64{"EUR": 1.08},
		Score: WineScoreConfig{MinScoreCount: 1, ShipTo: "ES"},
	}

	sources := []struct {
		id      string
		channel string
		origin  string
		raw     listing.Raw
	}{
		{"domaine-fr", "producer", "FR", rawWine("fr-1", "Domaine Example Bourgogne Rouge 2020 750ml", "WS 93", 3000, "EUR")},
		{"bodega-es", "retailer", "ES", rawWine("es-1", "Rioja Reserva 2018 750ml", "WS 92", 2500, "EUR")},
		{"napa-us", "producer", "US-CA", rawWine("us-1", "Napa Cabernet Sauvignon 2019 750ml", "WS 94", 6000, "USD")},
		{"bc-winery", "producer", "CA-BC", rawWine("ca-1", "Okanagan Pinot Noir 2021 750ml", "WS 91", 4000, "CAD")},
	}
	for _, s := range sources {
		ing, err := NewWineIngester(&fakeWineConn{id: s.id, raws: []listing.Raw{s.raw}},
			mustSource(t, s.channel, s.origin), deps)
		if err != nil {
			t.Fatalf("NewWineIngester(%s): %v", s.id, err)
		}
		if _, err := ing.Ingest(ctx); err != nil {
			t.Fatalf("Ingest(%s): %v", s.id, err)
		}
	}

	surfacedFor := func(t *testing.T, dest string) map[string]bool {
		t.Helper()
		d := deps
		d.Score.ShipTo = dest
		sr, err := NewWineSurface(d).Surface(ctx, store.Query{Category: "wine"})
		if err != nil {
			t.Fatalf("Surface(%s): %v", dest, err)
		}
		got := map[string]bool{}
		for _, sc := range sr.Items {
			got[sc.Item.SourceID] = true
		}
		return got
	}

	// A buyer in Spain: the Spanish retailer (domestic) and the French
	// producer (intra-EU distance selling). Not the US or Canadian sellers.
	es := surfacedFor(t, "ES")
	if !es["bodega-es"] || !es["domaine-fr"] {
		t.Errorf("ES buyer should see the ES retailer and the FR producer, got %v", es)
	}
	if es["napa-us"] || es["bc-winery"] {
		t.Errorf("ES buyer must not see third-country sellers by default, got %v", es)
	}

	// A buyer in Washington: only the US winery (winery-direct is broadly
	// permitted); no EU or Canadian seller may ship in.
	wa := surfacedFor(t, "US-WA")
	if !wa["napa-us"] {
		t.Errorf("US-WA buyer should see the US winery, got %v", wa)
	}
	if wa["domaine-fr"] || wa["bodega-es"] || wa["bc-winery"] {
		t.Errorf("US-WA buyer must not see foreign sellers, got %v", wa)
	}

	// A gift for someone in British Columbia: the BC winery only.
	bc := surfacedFor(t, "CA-BC")
	if !bc["bc-winery"] {
		t.Errorf("CA-BC buyer should see the BC winery, got %v", bc)
	}
	if bc["napa-us"] || bc["domaine-fr"] {
		t.Errorf("CA-BC buyer must not see cross-border sellers by default, got %v", bc)
	}

	// A buyer in Ontario: nothing here may legally reach them (the BC winery
	// cannot ship into ON), and an empty result is the correct answer.
	on := surfacedFor(t, "CA-ON")
	if len(on) != 0 {
		t.Errorf("CA-ON buyer should see nothing from this corpus, got %v", on)
	}
}

// TestWineSliceForeignCurrencyWithoutRate pins the other half of going
// international: a EUR listing must be reported unplaceable, never scored as
// though its number were USD.
func TestWineSliceForeignCurrencyWithoutRate(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	deps := WineDeps{Store: st, Score: WineScoreConfig{MinScoreCount: 1, ShipTo: "ES"}} // no Rates

	conn := &fakeWineConn{id: "bodega-es", raws: []listing.Raw{
		rawWine("es-1", "Rioja Reserva 2018 750ml", "WS 95", 2000, "EUR"),
	}}
	ing, err := NewWineIngester(conn, mustSource(t, "retailer", "ES"), deps)
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
		t.Fatalf("the listing should still surface, got %d", len(sr.Items))
	}
	if sr.Items[0].Signal.Verdict != "unknown-no-reference" {
		t.Fatalf("an unrated foreign currency must not be scored, got %q", sr.Items[0].Signal.Verdict)
	}

	// With a rate configured, the same listing is placed.
	deps.Rates = map[string]float64{"EUR": 1.08}
	sr, err = NewWineSurface(deps).Surface(ctx, store.Query{Category: "wine"})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if sr.Items[0].Signal.Verdict == "unknown-no-reference" {
		t.Fatalf("with a rate the listing should be placeable")
	}
}

// TestWineSliceRulesOverride pins the data-driven layer end to end: an
// operator override (WA legalizes out-of-state retailer shipping) changes
// what the SAME source may reach, with no code change.
func TestWineSliceRulesOverride(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()

	override := shipping.DefaultRules().Override(shipping.Rules{Destinations: map[string]shipping.Policy{
		"US-WA": {
			Producer: shipping.ChannelPolicy{SameSubdivision: true, SameCountry: true},
			Retailer: shipping.ChannelPolicy{SameSubdivision: true, SameCountry: true},
		},
	}})
	deps := WineDeps{
		Store: st,
		Ship:  &override,
		Score: WineScoreConfig{ShipTo: "US-WA", MinScoreCount: 1},
	}

	caConn := &fakeWineConn{id: "ca-wine", raws: []listing.Raw{
		rawWine("ca-deal", "Fine Cabernet Sauvignon 2019 750ml", "WS 94", 3000, "USD"),
	}}
	ing, err := NewWineIngester(caConn, mustSource(t, "retailer", "US-CA"), deps)
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
		rawWine("two-scores", "Fine Cabernet Sauvignon 2019", "WS 95 JS 96", 2000, "USD"),
	}}
	ing, err := NewWineIngester(conn, mustSource(t, "retailer", "US-WA"), deps)
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
