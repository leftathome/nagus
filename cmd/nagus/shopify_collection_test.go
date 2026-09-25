package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// nagus-bu2: a shopify source's `collection` reaches the connector, and a
// malformed handle fails at startup rather than on the first fetch.
func TestShopifyCollectionIsWiredAndValidated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "sources": [
	    {"name":"serverpartdeals","category":"hdd","type":"shopify","baseUrl":"https://example.test",
	     "collection":"all-hard-drives","productTypePrefixes":["Hard Drives","HDDs"],"intervalMinutes":60}
	  ],
	  "categories": {"hdd":{"minCapacityTB":6}}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRunConfig(path)
	if err != nil {
		t.Fatalf("LoadRunConfig: %v", err)
	}
	s := cfg.Sources[0]
	if s.Collection != "all-hard-drives" {
		t.Fatalf("collection = %q, want all-hard-drives", s.Collection)
	}
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		_, _ = w.Write([]byte(`{"products":[]}`))
	}))
	defer srv.Close()
	s.BaseURL = srv.URL
	conn, err := buildShopifyConnector(s, categoryOpts{})
	if err != nil {
		t.Fatalf("a valid collection must build: %v", err)
	}
	if _, err := conn.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != "/collections/all-hard-drives/products.json" {
		t.Fatalf("requested %q, want the configured collection's feed", got)
	}
	s.Collection = "../admin"
	if _, err := buildShopifyConnector(s, categoryOpts{}); err == nil {
		t.Fatal("a malformed collection handle must be refused at build time")
	}
}
