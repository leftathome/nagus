package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
