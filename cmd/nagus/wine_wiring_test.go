package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		State:       "WA",
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
	s.State = ""
	if _, err := buildIngester(s, CategoryConfig{}, store.NewMemoryStore(), categoryOpts{}); err == nil {
		t.Fatalf("a wine source without a home state must fail at startup")
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
	sf, err := buildSurface("wine", CategoryConfig{MinWineScore: 92, WineShipTo: "wa"}, store.NewMemoryStore(), categoryOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sf.Filter.Category != "wine" || sf.Filter.MinAttr["wine_score"] != 92 || sf.Filter.HasToken["ship_legal_to"] != "WA" {
		t.Errorf("surface filter not built from config: %+v", sf.Filter)
	}
}

func TestBuildSurfaceWineRejectsBadDestination(t *testing.T) {
	if _, err := buildSurface("wine", CategoryConfig{WineShipTo: "Cascadia"}, store.NewMemoryStore(), categoryOpts{}); err == nil {
		t.Fatalf("a non-state destination must be a startup error, not a silently-dark filter")
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
	doc := `{"destinations": {"WA": {"wineryDirect": true, "inStateRetailer": true, "outOfStateRetailer": true}}}`
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
	if !deps.Ship.Destinations["WA"].OutOfStateRetailer {
		t.Errorf("override should be merged over defaults, got %+v", deps.Ship.Destinations["WA"])
	}
	// Untouched destinations keep the full baseline table.
	if len(deps.Ship.Destinations) != 51 {
		t.Errorf("merged rules should keep the full table, got %d", len(deps.Ship.Destinations))
	}
}

func TestWineDepsFromBadShipRulesFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(path, []byte(`{"destinations": {"Wash": {}}}`), 0o600); err != nil {
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
		wineShipTo:        "CA",
	}
	cc := categoryConfigFromOpts("wine", o)
	if cc.BudgetCents != 5000 || cc.MinWineScore != 92 || cc.MinWineScoreCount != 2 || cc.WineShipTo != "CA" {
		t.Errorf("wine env opts not mapped onto CategoryConfig: %+v", cc)
	}
}

func TestLoadRunConfigAcceptsWineCategory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := `{
	  "categories": {"wine": {"minWineScore": 92, "wineShipTo": "WA"}},
	  "sources": [{
	    "name": "esquin", "category": "wine", "type": "shopify",
	    "baseUrl": "https://example-wine.test",
	    "wineChannel": "retailer", "state": "WA",
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
	if c.Sources[0].WineChannel != "retailer" || c.Sources[0].State != "WA" {
		t.Errorf("wine source declaration not parsed: %+v", c.Sources[0])
	}
	if got := c.Categories["wine"]; got.MinWineScore != 92 || got.WineShipTo != "WA" {
		t.Errorf("wine category config not parsed: %+v", got)
	}
}
