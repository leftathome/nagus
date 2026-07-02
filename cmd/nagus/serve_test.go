package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/leftathome/nagus/internal/category"
	"github.com/leftathome/nagus/internal/connector/ebay"
	"github.com/leftathome/nagus/internal/store"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	st := store.NewMemoryStore()
	conn := ebay.NewConnector(ebay.Config{FixturePath: "../../internal/connector/ebay/testdata/browse_search.json"})
	ref := category.StaticReference{CentsPerTB: map[string]int64{"new": 1900, "refurb": 1400, "used": 1150}}
	p := category.NewHDDPipeline(conn, category.HDDDeps{Store: st, Reference: ref})
	if _, err := p.Ingest(context.Background()); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}
	return &server{pipe: p, store: st, category: "hdd"}
}

func do(t *testing.T, srv *server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestServeHealthz(t *testing.T) {
	rec := do(t, newTestServer(t), http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("/healthz = %d %q", rec.Code, rec.Body.String())
	}
}

func TestServeSearchRanked(t *testing.T) {
	rec := do(t, newTestServer(t), http.MethodGet, "/search")
	if rec.Code != http.StatusOK {
		t.Fatalf("/search status = %d", rec.Code)
	}
	var body struct {
		Matched  int         `json:"matched"`
		Filtered int         `json:"filtered"`
		Items    []searchRow `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Matched != 3 || body.Filtered != 3 || len(body.Items) != 3 {
		t.Fatalf("matched=%d filtered=%d items=%d, want 3/3/3", body.Matched, body.Filtered, len(body.Items))
	}
	if body.Items[0].Verdict != "great" || body.Items[0].Condition != "used" {
		t.Fatalf("top item = {verdict=%s cond=%s}, want great/used", body.Items[0].Verdict, body.Items[0].Condition)
	}
	// Read-only surface: the untrusted title is returned as a data string.
	if body.Items[0].Title == "" {
		t.Fatal("expected the title carried as data")
	}
}

func TestServeSearchLimit(t *testing.T) {
	rec := do(t, newTestServer(t), http.MethodGet, "/search?limit=1")
	var body struct {
		Items []searchRow `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Items) != 1 {
		t.Fatalf("limit=1 returned %d items", len(body.Items))
	}
}

func TestServeGetItem(t *testing.T) {
	srv := newTestServer(t)
	// First find a real id via /search.
	rec := do(t, srv, http.MethodGet, "/search?limit=1")
	var body struct {
		Items []searchRow `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Items) != 1 {
		t.Fatalf("search seed failed: %v items=%d", err, len(body.Items))
	}
	id := body.Items[0].ID

	got := do(t, srv, http.MethodGet, "/item?id="+id)
	if got.Code != http.StatusOK {
		t.Fatalf("/item?id=%s status = %d", id, got.Code)
	}
	var it map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &it); err != nil {
		t.Fatalf("decode item: %v", err)
	}
	if it["id"] != id {
		t.Fatalf("get_item returned id %v, want %s", it["id"], id)
	}
}

func TestServeGetItemNotFoundAndBadRequest(t *testing.T) {
	srv := newTestServer(t)
	if rec := do(t, srv, http.MethodGet, "/item?id=does-not-exist"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing id status = %d, want 404", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/item"); rec.Code != http.StatusBadRequest {
		t.Fatalf("no id status = %d, want 400", rec.Code)
	}
}

func TestServeReadOnlyRejectsWrites(t *testing.T) {
	srv := newTestServer(t)
	// The surface is eyes-not-hands: non-GET is refused.
	if rec := do(t, srv, http.MethodPost, "/search"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /search status = %d, want 405", rec.Code)
	}
	if rec := do(t, srv, http.MethodDelete, "/item?id=x"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /item status = %d, want 405", rec.Code)
	}
}
