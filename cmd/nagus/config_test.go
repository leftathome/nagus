package main

import (
	"os"
	"path/filepath"
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
