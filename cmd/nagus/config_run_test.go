package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestResolveRunConfigConfigPathDelegatesToLoadRunConfig: when configPath is
// set, resolveRunConfig must return exactly what LoadRunConfig parses from it,
// ignoring cat/o/legacy entirely.
func TestResolveRunConfigConfigPathDelegatesToLoadRunConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "sources": [
	    {"name":"ebay","category":"hdd","type":"ebay","query":"internal hard drive","intervalMinutes":30}
	  ],
	  "categories": {"hdd":{"minCapacityTB":8},"land":{"minAcreageAcres":1}},
	  "defaultCategory": "hdd"
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// cat/o/legacy are deliberately mismatched (would error or synthesize
	// something else via the legacy path) to prove the config path short-circuits.
	rc, err := resolveRunConfig(path, "ghost", categoryOpts{}, legacySource{})
	if err != nil {
		t.Fatalf("resolveRunConfig: %v", err)
	}
	if len(rc.Sources) != 1 || rc.Sources[0].Name != "ebay" || rc.Sources[0].Category != "hdd" {
		t.Fatalf("sources = %+v, want the one parsed ebay/hdd source", rc.Sources)
	}
	if len(rc.Categories) != 2 {
		t.Fatalf("categories = %+v, want 2 (hdd,land)", rc.Categories)
	}
	if rc.DefaultCategory != "hdd" {
		t.Fatalf("defaultCategory = %q, want hdd", rc.DefaultCategory)
	}

	want, werr := LoadRunConfig(path)
	if werr != nil {
		t.Fatalf("LoadRunConfig: %v", werr)
	}
	if len(rc.Sources) != len(want.Sources) || len(rc.Categories) != len(want.Categories) || rc.DefaultCategory != want.DefaultCategory {
		t.Fatalf("resolveRunConfig(configPath) = %+v, want %+v (LoadRunConfig)", rc, want)
	}
}

// TestResolveRunConfigLegacyHDDWithInterval: no config.json, cat=hdd, a legacy
// source with interval>0 synthesizes exactly one ebay source bound to hdd.
func TestResolveRunConfigLegacyHDDWithInterval(t *testing.T) {
	o := categoryOpts{hddMinCapacity: 8}
	legacy := legacySource{
		interval:    30 * time.Minute,
		ebayQuery:   "internal hard drive",
		ebayFixture: "testdata/browse_search.json",
	}
	rc, err := resolveRunConfig("", "hdd", o, legacy)
	if err != nil {
		t.Fatalf("resolveRunConfig: %v", err)
	}
	if len(rc.Sources) != 1 {
		t.Fatalf("sources = %+v, want exactly 1", rc.Sources)
	}
	src := rc.Sources[0]
	if src.Type != "ebay" || src.Category != "hdd" {
		t.Fatalf("source = %+v, want type=ebay category=hdd", src)
	}
	if src.Query != "internal hard drive" || src.Fixture != "testdata/browse_search.json" {
		t.Fatalf("source query/fixture = %q/%q, want the legacy flag values", src.Query, src.Fixture)
	}
	if src.IntervalMinutes != 30 {
		t.Fatalf("source.IntervalMinutes = %d, want 30", src.IntervalMinutes)
	}
	if rc.DefaultCategory != "hdd" {
		t.Fatalf("defaultCategory = %q, want hdd", rc.DefaultCategory)
	}
	cc, ok := rc.Categories["hdd"]
	if !ok {
		t.Fatal("categories[hdd] missing")
	}
	if cc.MinCapacityTB != o.hddMinCapacity {
		t.Fatalf("categories[hdd].MinCapacityTB = %v, want %v (from o.hddMinCapacity)", cc.MinCapacityTB, o.hddMinCapacity)
	}
}

// TestResolveRunConfigLegacyLandIsSurfaceOnly: land has no acquisition connector
// since the Craigslist source was removed (nagus-hh5) and its replacement is not
// wired yet (nagus-hla), so the legacy path synthesizes ZERO sources for land even
// when an ingest interval is set -- while still building the land category surface
// with the acreage/budget fields from categoryOpts. Ingest is gone; surfacing is not.
func TestResolveRunConfigLegacyLandIsSurfaceOnly(t *testing.T) {
	o := categoryOpts{landMinAcreage: 1, landMaxAcreage: 40, landBudgetCents: 500000}
	rc, err := resolveRunConfig("", "land", o, legacySource{interval: time.Hour})
	if err != nil {
		t.Fatalf("resolveRunConfig: %v", err)
	}
	if len(rc.Sources) != 0 {
		t.Fatalf("sources = %+v, want 0 (land has no connector; surface-only)", rc.Sources)
	}
	cc, ok := rc.Categories["land"]
	if !ok {
		t.Fatal("categories[land] missing")
	}
	if cc.MinAcreageAcres != o.landMinAcreage || cc.MaxAcreageAcres != o.landMaxAcreage || cc.BudgetCents != o.landBudgetCents {
		t.Fatalf("categories[land] = %+v, want min=%v max=%v budget=%v", cc, o.landMinAcreage, o.landMaxAcreage, o.landBudgetCents)
	}
	if rc.DefaultCategory != "land" {
		t.Fatalf("defaultCategory = %q, want land", rc.DefaultCategory)
	}
}

// TestResolveRunConfigLegacySurfaceOnly: interval==0 means scheduled ingest is
// disabled, so the synthesized RunConfig has zero sources but still exactly
// one category (the surface still needs to be built).
func TestResolveRunConfigLegacySurfaceOnly(t *testing.T) {
	rc, err := resolveRunConfig("", "hdd", categoryOpts{hddMinCapacity: 8}, legacySource{interval: 0})
	if err != nil {
		t.Fatalf("resolveRunConfig: %v", err)
	}
	if len(rc.Sources) != 0 {
		t.Fatalf("sources = %+v, want 0 (surface-only)", rc.Sources)
	}
	if len(rc.Categories) != 1 {
		t.Fatalf("categories = %+v, want exactly 1", rc.Categories)
	}
	if rc.DefaultCategory != "hdd" {
		t.Fatalf("defaultCategory = %q, want hdd", rc.DefaultCategory)
	}
}

// TestResolveRunConfigUnsupportedCategoryErrors: the legacy path rejects a
// category the CLI doesn't know how to build.
func TestResolveRunConfigUnsupportedCategoryErrors(t *testing.T) {
	_, err := resolveRunConfig("", "ghost", categoryOpts{}, legacySource{interval: time.Minute})
	if err == nil {
		t.Fatal("expected error for unsupported category")
	}
}
