package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/category"
	"github.com/leftathome/nagus/internal/offer"
)

func TestLoadRunConfigParsesSourcesAndCategories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "sources": [
	    {"name":"ebay","category":"hdd","type":"ebay","query":"internal hard drive","intervalMinutes":30,"secretRef":"ebay"},
	    {"name":"land-src","category":"land","type":"ebay","intervalMinutes":60}
	  ],
	  "categories": {"hdd":{"minCapacityTB":8},"land":{"minAcreageAcres":1,"budgetCents":0}}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRunConfig(path)
	if err != nil {
		t.Fatalf("LoadRunConfig: %v", err)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("sources=%d want 2", len(cfg.Sources))
	}
	if cfg.Sources[0].Name != "ebay" || cfg.Sources[0].Category != "hdd" || cfg.Sources[0].IntervalMinutes != 30 {
		t.Fatalf("source0 = %+v", cfg.Sources[0])
	}
	if _, ok := cfg.Categories["land"]; !ok {
		t.Fatal("missing land category")
	}
}

func TestLoadRunConfigRejectsUnknownCategoryRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"sources":[{"name":"x","category":"ghost","type":"ebay"}],"categories":{"hdd":{}}}`
	_ = os.WriteFile(path, []byte(body), 0o600)
	if _, err := LoadRunConfig(path); err == nil {
		t.Fatal("expected error: source references a category not in categories{}")
	}
}

func TestLoadRunConfigRejectsDuplicateSourceName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"sources":[{"name":"dup","category":"hdd","type":"ebay"},{"name":"dup","category":"hdd","type":"ebay"}],"categories":{"hdd":{}}}`
	_ = os.WriteFile(path, []byte(body), 0o600)
	if _, err := LoadRunConfig(path); err == nil {
		t.Fatal("expected error: duplicate source name")
	}
}

// TestLoadRunConfigParsesZillapiSource: the land connector is anchored on a
// bounding box (upstream rejects a search without one), so the bbox and the
// per-poll spend caps must survive config parsing.
func TestLoadRunConfigParsesZillapiSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "sources": [
	    {"name":"north-sound","category":"land","type":"zillapi","intervalMinutes":1440,
	     "maxItems":25,"daysOnZillow":"7",
	     "bbox":{"west":-123.141452,"south":48.055894,"east":-121.868411,"north":48.673576}}
	  ],
	  "categories": {"land":{"minAcreageAcres":1,"budgetCents":20000000}}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRunConfig(path)
	if err != nil {
		t.Fatalf("LoadRunConfig: %v", err)
	}
	if len(cfg.Sources) != 1 {
		t.Fatalf("sources=%d want 1", len(cfg.Sources))
	}
	src := cfg.Sources[0]
	if src.Type != "zillapi" || src.IntervalMinutes != 1440 {
		t.Fatalf("source = %+v, want type=zillapi interval=1440", src)
	}
	if src.MaxItems != 25 || src.DaysOnZillow != "7" {
		t.Errorf("maxItems/daysOnZillow = %d/%q, want 25/\"7\"", src.MaxItems, src.DaysOnZillow)
	}
	if src.BBox == nil {
		t.Fatal("bbox must parse: a zillapi source without one cannot search")
	}
	if src.BBox.West != -123.141452 || src.BBox.South != 48.055894 || src.BBox.East != -121.868411 || src.BBox.North != 48.673576 {
		t.Errorf("bbox = %+v, want the four edges verbatim", *src.BBox)
	}
}

// A live zillapi source with no bbox must fail at build time with a clear
// message, not burn an upstream call on a guaranteed 400.
func TestBuildZillapiConnectorRequiresBBoxWhenLive(t *testing.T) {
	_, err := buildZillapiConnector(
		SourceConfig{Name: "no-box", Category: "land", Type: "zillapi"},
		CategoryConfig{MinAcreageAcres: 1},
		categoryOpts{zillapiKey: "zk_x"},
	)
	if err == nil {
		t.Fatal("want an error when a live zillapi source has no bbox")
	}
	if !strings.Contains(err.Error(), "bbox") {
		t.Errorf("err = %v, want it to name the missing bbox", err)
	}
}

// A fixture-backed source needs neither bbox nor key: that is the offline path.
func TestBuildZillapiConnectorFixtureNeedsNoCreds(t *testing.T) {
	conn, err := buildZillapiConnector(
		SourceConfig{Name: "off", Category: "land", Type: "zillapi", Fixture: "../../internal/connector/zillapi/testdata/search_lots.json"},
		CategoryConfig{MinAcreageAcres: 1},
		categoryOpts{},
	)
	if err != nil {
		t.Fatalf("fixture source must build without creds: %v", err)
	}
	if got := conn.SourceID(); got != "zillapi:off" {
		t.Errorf("SourceID = %q, want zillapi:off", got)
	}
}

// Retention is a property of the SOURCE's terms, not of the category. Before
// nagus-q6u every hdd source inherited eBay's 6h purge, which applied eBay's
// License 8.1(b) obligation to storefronts that have no such restriction -- and
// made them fragile, since a few hours of rate-limiting would have wiped a
// storefront's whole corpus.
func TestRetentionIsPerSourceNotPerCategory(t *testing.T) {
	ebay := SourceConfig{Name: "ebay", Category: "hdd", Type: "ebay", IntervalMinutes: 30}
	shop := SourceConfig{Name: "spd", Category: "hdd", Type: "shopify", IntervalMinutes: 60}

	eStale, eRet, eExp := retentionForSource(ebay)
	if eStale != category.EbayContentMaxAge {
		t.Errorf("ebay StaleAfter = %v, want the 6h content window", eStale)
	}
	if eRet.Policy != offer.Purge || eRet.Window != category.EbayContentMaxAge {
		t.Errorf("ebay offer retention = %+v, want purge on the 6h window", eRet)
	}

	sStale, sRet, sExp := retentionForSource(shop)
	if sStale != 0 {
		t.Errorf("shopify StaleAfter = %v, want 0 -- a storefront has no obligation to forget", sStale)
	}
	if sRet.Policy != offer.RetainFull {
		t.Errorf("shopify offer retention = %+v, want retain-full", sRet)
	}

	// Expiry is a grace period of several polls so ONE failed poll never marks a
	// live catalogue dead -- which matters, storefronts rate-limit hard.
	if eExp != 3*30*time.Minute {
		t.Errorf("ebay expireAfter = %v, want 3 poll intervals", eExp)
	}
	if sExp != 3*time.Hour {
		t.Errorf("shopify expireAfter = %v, want 3 poll intervals", sExp)
	}
	if sExp <= 60*time.Minute {
		t.Error("expiry grace must exceed one poll interval, or a single miss expires everything")
	}
}

// summarize-decay is the intended eBay end state but must stay OFF until its
// summary schema is validated against eBay's terms.
func TestEbayDoesNotUseSummarizeDecayYet(t *testing.T) {
	_, ret, _ := retentionForSource(SourceConfig{Type: "ebay", IntervalMinutes: 30})
	if ret.Policy == offer.SummarizeDecay {
		t.Fatal("eBay must not use summarize-decay until the summary schema is validated as compliant")
	}
}
