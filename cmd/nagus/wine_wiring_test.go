package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/shipping"
	"github.com/leftathome/nagus/internal/store"
)

// wineSource returns a minimal valid wine source config (fixture-driven
// shopify storefront, WA retailer).
func wineSource() SourceConfig {
	return SourceConfig{
		Name:        "esquin",
		Category:    "wine",
		Type:        "shopify",
		Fixture:     "../../internal/connector/shopify/testdata/products.json",
		WineChannel: "retailer",
		Origin:      "US-WA",
	}
}

func TestBuildIngesterWineRequiresDeclaration(t *testing.T) {
	s := wineSource()
	s.WineChannel = ""
	_, err := buildIngester(s, CategoryConfig{}, store.NewMemoryStore(), categoryOpts{})
	if err == nil {
		t.Fatalf("a wine source without wineChannel must fail at startup")
	}
	if !strings.Contains(err.Error(), "wineChannel") {
		t.Errorf("error should point the operator at wineChannel, got: %v", err)
	}

	s = wineSource()
	s.WineChannel = "mail_fraud"
	if _, err := buildIngester(s, CategoryConfig{}, store.NewMemoryStore(), categoryOpts{}); err == nil {
		t.Fatalf("an unknown wineChannel must fail at startup")
	}

	s = wineSource()
	s.Origin = ""
	if _, err := buildIngester(s, CategoryConfig{}, store.NewMemoryStore(), categoryOpts{}); err == nil {
		t.Fatalf("a wine source without an origin jurisdiction must fail at startup")
	}

	s = wineSource()
	s.Origin = "Cascadia"
	if _, err := buildIngester(s, CategoryConfig{}, store.NewMemoryStore(), categoryOpts{}); err == nil {
		t.Fatalf("a malformed origin jurisdiction must fail at startup")
	}
}

func TestBuildIngesterWineInternationalSource(t *testing.T) {
	s := wineSource()
	s.Name = "domaine"
	s.WineChannel = "producer"
	s.Origin = "FR"
	if _, err := buildIngester(s, CategoryConfig{}, store.NewMemoryStore(), categoryOpts{}); err != nil {
		t.Fatalf("a country-level international source should build: %v", err)
	}
}

func TestBuildIngesterWineWithValidDeclaration(t *testing.T) {
	ing, err := buildIngester(wineSource(), CategoryConfig{}, store.NewMemoryStore(), categoryOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The channel tagger must pass through the inner connector's id (the
	// shopify connector namespaces it as "shopify:<name>").
	if ing.Connector.SourceID() != "shopify:esquin" {
		t.Errorf("channel tagger must preserve the source id, got %q", ing.Connector.SourceID())
	}
}

func TestBuildSurfaceWine(t *testing.T) {
	sf, err := buildSurface("wine", CategoryConfig{MinWineScore: 92, WineShipTo: "us-wa"}, store.NewMemoryStore(), categoryOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sf.Filter.Category != "wine" || sf.Filter.MinAttr["wine_score"] != 92 || sf.Filter.HasToken["ship_legal_to"] != "US-WA" {
		t.Errorf("surface filter not built from config: %+v", sf.Filter)
	}
}

func TestBuildSurfaceWineInternationalDestination(t *testing.T) {
	sf, err := buildSurface("wine", CategoryConfig{WineShipTo: "fr"}, store.NewMemoryStore(), categoryOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sf.Filter.HasToken["ship_legal_to"] != "FR" {
		t.Errorf("a country destination should normalize, got %+v", sf.Filter.HasToken)
	}
}

func TestBuildSurfaceWineRejectsUnmodeledDestination(t *testing.T) {
	// Norway is deliberately absent from the baseline table: accepting it
	// would darken the surface with no explanation, so it fails at startup.
	_, err := buildSurface("wine", CategoryConfig{WineShipTo: "NO"}, store.NewMemoryStore(), categoryOpts{})
	if err == nil {
		t.Fatalf("an unmodeled destination must be a startup error")
	}
	if !strings.Contains(err.Error(), "rules override") {
		t.Errorf("the error should point at the override path, got: %v", err)
	}
}

func TestBuildSurfaceWineAcceptsDestinationAddedByOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	doc := `{"destinations": {"NO": {"producer": {"sameCountry": true, "sameBloc": true},
	                                 "retailer": {"sameCountry": true}}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildSurface("wine", CategoryConfig{WineShipTo: "NO"}, store.NewMemoryStore(), categoryOpts{wineShipRules: path}); err != nil {
		t.Fatalf("an override should make the destination usable: %v", err)
	}
}

func TestWineDepsFromCarriesFXRates(t *testing.T) {
	deps, err := wineDepsFrom(CategoryConfig{WineFXRates: map[string]float64{"EUR": 1.08}}, store.NewMemoryStore(), categoryOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deps.Rates["EUR"] != 1.08 {
		t.Errorf("FX rates should reach the bundle, got %v", deps.Rates)
	}
}

func TestBuildSurfaceWineRejectsBadDestination(t *testing.T) {
	if _, err := buildSurface("wine", CategoryConfig{WineShipTo: "Cascadia"}, store.NewMemoryStore(), categoryOpts{}); err == nil {
		t.Fatalf("a malformed destination must be a startup error, not a silently-dark filter")
	}
}

func TestWineDepsFromLoadsLWIN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lwin.csv")
	csv := "LWIN,PRODUCER_NAME,WINE,COUNTRY,REGION,COLOUR\n1101245,Leonetti Cellar,Cabernet Sauvignon,USA,Walla Walla,Red\n"
	if err := os.WriteFile(path, []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}
	deps, err := wineDepsFrom(CategoryConfig{}, store.NewMemoryStore(), categoryOpts{lwinCSV: path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deps.LWIN == nil || deps.LWIN.DB.Len() != 1 {
		t.Fatalf("expected a loaded LWIN resolver, got %+v", deps.LWIN)
	}
}

func TestWineDepsFromMissingLWINFileFailsLoudly(t *testing.T) {
	_, err := wineDepsFrom(CategoryConfig{}, store.NewMemoryStore(), categoryOpts{lwinCSV: "/does/not/exist.csv"})
	if err == nil {
		t.Fatalf("a configured-but-missing LWIN export must be a startup error, not a silent identity-less run")
	}
}

func TestWineDepsFromLoadsShipRulesOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	doc := `{"destinations": {"US-WA": {"producer": {"sameSubdivision": true, "sameCountry": true},
	                                   "retailer": {"sameSubdivision": true, "sameCountry": true}}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	deps, err := wineDepsFrom(CategoryConfig{}, store.NewMemoryStore(), categoryOpts{wineShipRules: path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deps.Ship == nil {
		t.Fatalf("expected a loaded rules override")
	}
	if !deps.Ship.Destinations["US-WA"].Retailer.SameCountry {
		t.Errorf("override should be merged over defaults, got %+v", deps.Ship.Destinations["US-WA"])
	}
	// Untouched destinations keep the full baseline table.
	if len(deps.Ship.Destinations) != len(shipping.DefaultRules().Destinations) {
		t.Errorf("merged rules should keep the full table, got %d", len(deps.Ship.Destinations))
	}
}

func TestWineDepsFromBadShipRulesFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(path, []byte(`{"destinations": {"Wash": {"producer": {}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := wineDepsFrom(CategoryConfig{}, store.NewMemoryStore(), categoryOpts{wineShipRules: path}); err == nil {
		t.Fatalf("a malformed rules file must be a startup error")
	}
	if _, err := wineDepsFrom(CategoryConfig{}, store.NewMemoryStore(), categoryOpts{wineShipRules: "/does/not/exist.json"}); err == nil {
		t.Fatalf("a configured-but-missing rules file must be a startup error")
	}
}

func TestCategoryConfigFromOptsWine(t *testing.T) {
	o := categoryOpts{
		wineBudgetCents:   5000,
		wineMinScore:      92,
		wineMinScoreCount: 2,
		wineShipTo:        "CA-BC",
	}
	cc := categoryConfigFromOpts("wine", o)
	if cc.BudgetCents != 5000 || cc.MinWineScore != 92 || cc.MinWineScoreCount != 2 || cc.WineShipTo != "CA-BC" {
		t.Errorf("wine env opts not mapped onto CategoryConfig: %+v", cc)
	}
}

func TestLoadRunConfigAcceptsWineCategory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := `{
	  "categories": {"wine": {"minWineScore": 92, "wineShipTo": "US-WA",
	                          "wineFxRates": {"EUR": 1.08}}},
	  "sources": [{
	    "name": "esquin", "category": "wine", "type": "shopify",
	    "baseUrl": "https://example-wine.test",
	    "wineChannel": "retailer", "origin": "US-WA",
	    "intervalMinutes": 360
	  }]
	}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadRunConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Sources[0].WineChannel != "retailer" || c.Sources[0].Origin != "US-WA" {
		t.Errorf("wine source declaration not parsed: %+v", c.Sources[0])
	}
	got := c.Categories["wine"]
	if got.MinWineScore != 92 || got.WineShipTo != "US-WA" {
		t.Errorf("wine category config not parsed: %+v", got)
	}
	if got.WineFXRates["EUR"] != 1.08 {
		t.Errorf("FX rates not parsed: %+v", got.WineFXRates)
	}
}

// TestShippedWineExamplesLoad pins the docs/examples files against the real
// loaders. A rules file is JSON with no comments and unknown fields
// rejected, and a config that no longer parses is worse than no example --
// both rot silently otherwise.
func TestShippedWineExamplesLoad(t *testing.T) {
	cfg, err := LoadRunConfig("../../docs/examples/config.wine.json")
	if err != nil {
		t.Fatalf("shipped config.wine.json must load: %v", err)
	}
	for _, s := range cfg.Sources {
		if _, err := shipping.NewSource(s.WineChannel, s.Origin); err != nil {
			t.Errorf("example source %q has an invalid declaration: %v", s.Name, err)
		}
	}
	wineCC, ok := cfg.Categories["wine"]
	if !ok {
		t.Fatalf("the example should configure the wine category")
	}
	if _, err := wineDepsFrom(wineCC, store.NewMemoryStore(), categoryOpts{}); err != nil {
		t.Errorf("the example category config must build deps: %v", err)
	}

	f, err := os.Open("../../docs/examples/wine-ship-rules.json")
	if err != nil {
		t.Fatalf("opening the shipped rules example: %v", err)
	}
	defer f.Close()
	override, err := shipping.LoadRules(f)
	if err != nil {
		t.Fatalf("shipped wine-ship-rules.json must load: %v", err)
	}
	// And it must actually do something: the AU override opens third-country
	// shipping the baseline keeps closed.
	rules := shipping.DefaultRules().Override(override)
	frShop, err := shipping.NewSource("retailer", "FR")
	if err != nil {
		t.Fatal(err)
	}
	if !rules.Legal(frShop, "AU") {
		t.Errorf("the example override should open AU to foreign retailers")
	}
}
