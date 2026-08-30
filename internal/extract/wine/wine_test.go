package wine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/identity/lwin"
	"github.com/leftathome/nagus/internal/listing"
)

func sanitized(title, body string) listing.Sanitized {
	return listing.Sanitized{
		SourceID:   "totalwine",
		SourceKey:  "sku-123",
		SourceURL:  "https://example.com/p/123",
		Title:      title,
		Body:       body,
		PriceCents: 4299,
		Currency:   "USD",
		SeenAt:     time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		Boundary:   "sanitize.passthrough(wine)",
	}
}

func TestExtract_FullRetailListing(t *testing.T) {
	e := New()
	s := sanitized("Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", "WS 94 JS 95 JD 96 - a benchmark Walla Walla cab.")
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if it.Category != "wine" || it.Class != "durable" {
		t.Errorf("category/class wrong: %s/%s", it.Category, it.Class)
	}
	if it.Condition != "new" {
		t.Errorf("retail wine should be condition new, got %q", it.Condition)
	}
	checks := map[string]string{
		"vintage":          "2019",
		"bottle_ml":        "750",
		"varietal":         "Cabernet Sauvignon",
		"colour":           "red",
		"wine_score":       "95.0",
		"wine_score_count": "3",
		"critic_scores":    "JD:96 JS:95 WS:94",
	}
	for k, want := range checks {
		if got := it.Attributes[k]; got != want {
			t.Errorf("attribute %q = %q, want %q", k, got, want)
		}
	}
	if it.PriceCents != 4299 || it.Currency != "USD" {
		t.Errorf("price passthrough wrong: %d %s", it.PriceCents, it.Currency)
	}
}

func TestExtract_DeterministicID(t *testing.T) {
	e := New()
	a, err := e.Extract(context.Background(), sanitized("Some Wine 2020", ""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := e.Extract(context.Background(), sanitized("Some Wine 2020 (relisted)", ""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("same source+key must produce the same id")
	}
	other := sanitized("Some Wine 2020", "")
	other.SourceKey = "sku-999"
	c, err := e.Extract(context.Background(), other)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.ID == c.ID {
		t.Errorf("different source keys must produce different ids")
	}
}

func TestExtract_MissingSourceKeyErrors(t *testing.T) {
	e := New()
	s := sanitized("Wine 2020", "")
	s.SourceKey = ""
	if _, err := e.Extract(context.Background(), s); err == nil {
		t.Fatalf("missing provenance must be an extraction error")
	}
}

func TestExtract_ChannelAspectsLiftedAndValidated(t *testing.T) {
	e := New()
	s := sanitized("Wine 2020", "")
	s.Aspects = map[string]string{
		"wine_channel":  "producer",
		"source_origin": "us-wa",
		"ship_legal_to": "US-CA US-OR US-WA",
	}
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if it.Attributes["wine_channel"] != "producer" {
		t.Errorf("channel should be lifted, got %v", it.Attributes)
	}
	if it.Attributes["source_origin"] != "US-WA" {
		t.Errorf("source origin should be lifted normalized, got %q", it.Attributes["source_origin"])
	}
	if it.Attributes["ship_legal_to"] != "US-CA US-OR US-WA" {
		t.Errorf("legal destinations should be lifted, got %q", it.Attributes["ship_legal_to"])
	}
}

func TestExtract_InternationalJurisdictionsLifted(t *testing.T) {
	e := New()
	s := sanitized("Chateau Something 2019", "")
	s.Aspects = map[string]string{
		"wine_channel":  "producer",
		"source_origin": "fr",
		"ship_legal_to": "AT BE FR ES IT",
	}
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if it.Attributes["source_origin"] != "FR" {
		t.Errorf("country-level origin should lift, got %q", it.Attributes["source_origin"])
	}
	if it.Attributes["ship_legal_to"] != "AT BE FR ES IT" {
		t.Errorf("country-level destinations should lift, got %q", it.Attributes["ship_legal_to"])
	}
}

func TestExtract_ShipLegalToTokensValidated(t *testing.T) {
	// Aspect values are untrusted: malformed tokens must never reach the
	// destination filter's token space, and an invalid source_origin is
	// dropped rather than lifted.
	e := New()
	s := sanitized("Wine 2020", "")
	s.Aspects = map[string]string{
		"source_origin": "Cascadia",
		"ship_legal_to": "US-CA ANYWHERE fr true US-WASH 12 CA-BC",
	}
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := it.Attributes["source_origin"]; present {
		t.Errorf("invalid source_origin must be dropped, got %q", it.Attributes["source_origin"])
	}
	if it.Attributes["ship_legal_to"] != "US-CA FR CA-BC" {
		t.Errorf("only well-formed jurisdiction tokens may survive, got %q", it.Attributes["ship_legal_to"])
	}
}

func TestExtract_EmptyShipLegalToIsStampedNotDropped(t *testing.T) {
	// "Ships nowhere legally" is a real fail-closed fact, distinct from an
	// untagged source (no aspect at all).
	e := New()
	s := sanitized("Wine 2020", "")
	s.Aspects = map[string]string{"ship_legal_to": ""}
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, present := it.Attributes["ship_legal_to"]
	if !present || v != "" {
		t.Errorf("empty legal set should be stamped as empty, got present=%v value=%q", present, v)
	}

	// No aspect -> no attribute.
	s.Aspects = nil
	it, err = e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := it.Attributes["ship_legal_to"]; present {
		t.Errorf("untagged source must carry no ship_legal_to attribute")
	}
}

func TestExtract_LWINResolverStampsOnlyAutoRoute(t *testing.T) {
	db := lwin.NewDB([]lwin.Record{
		{LWIN7: "1101245", Producer: "Leonetti Cellar", Wine: "Cabernet Sauvignon", Region: "Walla Walla", Colour: "red"},
	})
	e := &Extractor{Resolver: &lwin.Resolver{DB: db}}

	it, err := e.Extract(context.Background(), sanitized("Leonetti Cellar Cabernet Sauvignon 2019 750ml", ""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if it.CanonicalID != "11012452019" {
		t.Errorf("high-confidence match should stamp LWIN-11, got %q", it.CanonicalID)
	}
	if it.Attributes["lwin_route"] != "auto" {
		t.Errorf("route should be recorded, got %q", it.Attributes["lwin_route"])
	}

	// A non-matching listing must stay unidentified with the route recorded.
	it, err = e.Extract(context.Background(), sanitized("Screaming Eagle Napa 2018", ""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if it.CanonicalID != "" {
		t.Errorf("low-confidence must never stamp an identity, got %q", it.CanonicalID)
	}
	if it.Attributes["lwin_route"] != "review" {
		t.Errorf("expected review route, got %q", it.Attributes["lwin_route"])
	}
}

func TestExtract_NoResolverLeavesNoRoute(t *testing.T) {
	e := New()
	it, err := e.Extract(context.Background(), sanitized("Wine 2020", ""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := it.Attributes["lwin_route"]; present {
		t.Errorf("no resolver should record no route")
	}
}

// --- vintage ---

func TestExtractVintage(t *testing.T) {
	cases := []struct {
		text string
		want int
		ok   bool
	}{
		{"Chateau Margaux 2015", 2015, true},
		{"Vintage Port 1977", 1977, true},
		{"NV Brut Champagne", 0, false},
		{"No year here", 0, false},
		{"1500ml bottle", 0, false},        // size, not vintage
		{"Warehouse item 20150", 0, false}, // not a bounded year
	}
	for _, c := range cases {
		got, ok := extractVintage(c.text)
		if got != c.want || ok != c.ok {
			t.Errorf("extractVintage(%q) = %d,%v want %d,%v", c.text, got, ok, c.want, c.ok)
		}
	}
}

// --- bottle size ---

func TestExtractBottleML(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"Some Wine 750ml", 750},
		{"Some Wine 375 ml", 375},
		{"Some Wine 1.5L", 1500},
		{"Some Wine Magnum", 1500},
		{"Double Magnum of Wine", 3000},
		{"Half Bottle Sauternes", 375},
		{"Plain title", DefaultBottleML},
	}
	for _, c := range cases {
		if got := extractBottleML(c.text); got != c.want {
			t.Errorf("extractBottleML(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

// --- varietal / colour ---

func TestExtractVarietal(t *testing.T) {
	v, colour, ok := extractVarietal("2019 Walla Walla Cabernet Sauvignon")
	if !ok || v != "Cabernet Sauvignon" || colour != "red" {
		t.Errorf("got %q/%q/%v", v, colour, ok)
	}
	// Longer names must win over substrings: "sauvignon blanc" is white, and
	// must not be caught by a shorter red entry.
	v, colour, ok = extractVarietal("Cakebread Sauvignon Blanc 2022")
	if !ok || v != "Sauvignon Blanc" || colour != "white" {
		t.Errorf("got %q/%q/%v", v, colour, ok)
	}
	// Accent folding.
	v, _, ok = extractVarietal("Trimbach Gewürztraminer 2020")
	if !ok || v != "Gewurztraminer" {
		t.Errorf("accented varietal should fold, got %q/%v", v, ok)
	}
	if _, _, ok := extractVarietal("A mystery red"); ok {
		t.Errorf("unknown varietal should not match")
	}
}

func TestExtractColour_Fallback(t *testing.T) {
	c, ok := extractColour("NV Brut Champagne")
	if !ok || c != "sparkling" {
		t.Errorf("champagne should be sparkling, got %q/%v", c, ok)
	}
	c, ok = extractColour("Provence Rosé 2023")
	if !ok || c != "rose" {
		t.Errorf("rosé should fold and match, got %q/%v", c, ok)
	}
}

// --- critic parsing ---

func TestParseCriticScores_Shorthand(t *testing.T) {
	raw := parseCriticScores("WS 92, JS-94, RP: 95+, JD 96 pts")
	got := map[string]float64{}
	for _, r := range raw {
		got[r.Critic] = r.Score
	}
	want := map[string]float64{"WS": 92, "JS": 94, "RP": 95, "JD": 96}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("critic %s = %v, want %v (raw %v)", k, got[k], v, raw)
		}
	}
}

func TestParseCriticScores_FullNamesAndJR(t *testing.T) {
	raw := parseCriticScores("Wine Spectator 93 points; Jancis Robinson 17.5; The Wine Advocate: 95")
	var ws, jr, rp bool
	for _, r := range raw {
		switch r.Critic {
		case "WS":
			ws = r.Score == 93 && r.Scale == 100
		case "JR":
			jr = r.Score == 17.5 && r.Scale == 20
		case "RP":
			rp = r.Score == 95 && r.Scale == 100
		}
	}
	if !ws || !jr || !rp {
		t.Errorf("full-name parsing failed: %v", raw)
	}
}

func TestParseCriticScores_LowercaseCodeIgnored(t *testing.T) {
	// Case-sensitivity on shorthand codes is deliberate: prose "ws 92" is
	// more likely noise than an attribution.
	raw := parseCriticScores("this ws 92 in the cellar")
	if len(raw) != 0 {
		t.Errorf("lowercase shorthand must not parse, got %v", raw)
	}
}

func TestParseCriticScores_WACodeNotRecognized(t *testing.T) {
	// "WA" is Washington state in this project's home market ("Seattle, WA
	// 98101"); The Wine Advocate is RP / full name only.
	raw := parseCriticScores("Ships from Seattle, WA 98101")
	if len(raw) != 0 {
		t.Errorf("WA shorthand must not parse as a critic, got %v", raw)
	}
}

func TestExtract_DuplicateAttributionCountsOnce(t *testing.T) {
	e := New()
	s := sanitized("Big Cab 2019 WS 92", "WS 92 - Wine Spectator 92. A fine wine.")
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if it.Attributes["wine_score_count"] != "1" {
		t.Errorf("one critic repeated three ways must count once, got %q (scores %q)",
			it.Attributes["wine_score_count"], it.Attributes["critic_scores"])
	}
}

func TestExtract_NoScoresLeavesQualityAbsent(t *testing.T) {
	e := New()
	it, err := e.Extract(context.Background(), sanitized("Unscored Table Wine 2022", ""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, k := range []string{"wine_score", "wine_score_count", "critic_scores"} {
		if _, present := it.Attributes[k]; present {
			t.Errorf("attribute %q should be absent without attributions", k)
		}
	}
}

func TestTokenize(t *testing.T) {
	tokens := tokenize("Leonetti Cellar Cabernet Sauvignon 2019, 750ml!")
	joined := strings.Join(tokens, " ")
	for _, want := range []string{"leonetti", "cellar", "cabernet", "sauvignon", "2019", "750ml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tokens missing %q: %v", want, tokens)
		}
	}
}
