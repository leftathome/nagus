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

// TestResolveRunConfigLegacyLandWithInterval: cat=land synthesizes exactly one
// craigslist source bound to land, and the land category config carries the
// acreage/budget fields from categoryOpts.
func TestResolveRunConfigLegacyLandWithInterval(t *testing.T) {
	o := categoryOpts{landMinAcreage: 1, landMaxAcreage: 40, landBudgetCents: 500000}
	legacy := legacySource{
		interval:   time.Hour,
		clCity:     "seattle",
		clCategory: "reo",
		clFixture:  "",
	}
	rc, err := resolveRunConfig("", "land", o, legacy)
	if err != nil {
		t.Fatalf("resolveRunConfig: %v", err)
	}
	if len(rc.Sources) != 1 {
		t.Fatalf("sources = %+v, want exactly 1", rc.Sources)
	}
	src := rc.Sources[0]
	if src.Type != "craigslist" || src.Category != "land" {
		t.Fatalf("source = %+v, want type=craigslist category=land", src)
	}
	if src.City != "seattle" || src.ClCategory != "reo" {
		t.Fatalf("source city/clCategory = %q/%q, want seattle/reo", src.City, src.ClCategory)
	}
	if src.IntervalMinutes != 60 {
		t.Fatalf("source.IntervalMinutes = %d, want 60", src.IntervalMinutes)
	}
	cc, ok := rc.Categories["land"]
	if !ok {
		t.Fatal("categories[land] missing")
	}
	if cc.MinAcreageAcres != o.landMinAcreage || cc.MaxAcreageAcres != o.landMaxAcreage || cc.BudgetCents != o.landBudgetCents {
		t.Fatalf("categories[land] = %+v, want min=%v max=%v budget=%v", cc, o.landMinAcreage, o.landMaxAcreage, o.landBudgetCents)
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
