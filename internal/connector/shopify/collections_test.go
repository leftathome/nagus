package shopify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// collectionStore serves /collections/<handle>/products.json from a map of
// handle -> product ids (limit-paged), and counts requests per path.
func collectionStore(t *testing.T, colls map[string][]int) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?page="+r.URL.Query().Get("page"))
		h := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/collections/"), "/products.json")
		limit, page := 0, 0
		_, _ = fmt.Sscan(r.URL.Query().Get("limit"), &limit)
		_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
		ids := colls[h]
		var parts []string
		for i := (page - 1) * limit; i < page*limit && i < len(ids); i++ {
			n := ids[i]
			parts = append(parts, fmt.Sprintf(`{"id":%d,"title":"D%d","handle":"d%d","product_type":"HDDs > 18TB","variants":[{"id":%d,"price":"10.00","available":true}]}`, n, n, n, 1000+n))
		}
		_, _ = w.Write([]byte(`{"products":[` + strings.Join(parts, ",") + `]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

func collConnector(url string, colls []string, maxPages int, logf func(string, ...any)) *Connector {
	return NewConnector(Config{
		Name: "s", BaseURL: url, Collections: colls, Limit: 2, MaxPages: maxPages, PageDelay: -1,
		ProductTypePrefixes: []string{"Hard Drives", "HDDs"}, Now: func() time.Time { return fixedNow }, Logf: logf,
	})
}

// Several collections are all walked, in order; a product in two of them
// (serverpartdeals files most drives in both "all-hard-drives" and
// "hard-drives") is emitted once; and the run is complete when both are.
func TestCollectionsAreAllWalkedAndDeduped(t *testing.T) {
	srv, paths := collectionStore(t, map[string][]int{
		"all-hard-drives": {1, 2, 3},
		"hard-drives":     {2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, // 7 HDDs-typed drives only here
	})
	c := collConnector(srv.URL, []string{"all-hard-drives", "hard-drives"}, 10, nil)
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(raws) != 11 {
		t.Fatalf("raws = %d, want 11 distinct products", len(raws))
	}
	keys := map[string]bool{}
	for _, r := range raws {
		if keys[r.SourceKey] {
			t.Fatalf("duplicate %s", r.SourceKey)
		}
		keys[r.SourceKey] = true
	}
	if !c.FetchComplete() {
		t.Fatal("both collections walked to the end: want complete")
	}
	if (*paths)[0] != "/collections/all-hard-drives/products.json?page=1" || (*paths)[len(*paths)-1] != "/collections/hard-drives/products.json?page=6" {
		t.Fatalf("paths %v", *paths)
	}
	// Collection and Collections together: the single one first, no repeat.
	c2 := NewConnector(Config{Name: "s", BaseURL: srv.URL, Collection: "hard-drives", Collections: []string{"hard-drives", "all-hard-drives"}, PageDelay: -1, Now: func() time.Time { return fixedNow }})
	if got := c2.walks(); len(got) != 2 || got[0] != "hard-drives" || got[1] != "all-hard-drives" {
		t.Fatalf("walks = %v", got)
	}
}

// One truncated collection makes the whole run incomplete.
func TestCollectionsCompleteOnlyIfEveryWalkIs(t *testing.T) {
	srv, _ := collectionStore(t, map[string][]int{
		"a": {1, 2, 3},
		"b": {4, 5, 6, 7, 8, 9}, // 3 full pages at limit 2: truncated by MaxPages 2
	})
	var logged []string
	c := collConnector(srv.URL, []string{"a", "b"}, 2, func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if c.FetchComplete() {
		t.Fatal("collection b stopped at the cap: the run must be incomplete")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], `collection "b" TRUNCATED`) {
		t.Fatalf("log = %v, want one truncation warning naming collection b", logged)
	}
	// Order does not matter: a truncated FIRST collection also counts.
	c = collConnector(srv.URL, []string{"b", "a"}, 2, nil)
	if _, err := c.Fetch(context.Background()); err != nil || c.FetchComplete() {
		t.Fatalf("err=%v complete=%v, want incomplete", err, c.FetchComplete())
	}
}

// Reviewer finding 1: an EMPTY collection (emptied, hidden or renamed --
// Shopify answers an unknown handle with an empty list) is NOT a complete
// walk, or offer expiry would retire every offer the source holds.
func TestEmptyCollectionIsIncompleteAndWarned(t *testing.T) {
	srv, _ := collectionStore(t, map[string][]int{"a": {1, 2, 3}})
	var logged []string
	c := collConnector(srv.URL, []string{"a", "renamed"}, 10, func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(raws) != 3 {
		t.Fatalf("raws = %d, want collection a's 3", len(raws))
	}
	if c.FetchComplete() {
		t.Fatal("an empty collection must make the run incomplete")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], `collection "renamed" returned NO products on page 1`) {
		t.Fatalf("log = %v, want a warning naming the empty collection", logged)
	}
	// The whole-catalogue walk keeps its old meaning: an empty store is
	// simply empty (not a collection that vanished).
	empty, _ := pagedStore(t, 0, nil)
	cat := NewConnector(Config{Name: "s", BaseURL: empty.URL, PageDelay: -1, Now: func() time.Time { return fixedNow }})
	if _, err := cat.Fetch(context.Background()); err != nil || !cat.FetchComplete() {
		t.Fatalf("catalogue: err=%v complete=%v, want complete", err, cat.FetchComplete())
	}
	// An empty collection past page 1 is the ordinary end of the walk.
	srv2, _ := collectionStore(t, map[string][]int{"a": {1, 2}}) // one full page at limit 2, then empty
	c2 := collConnector(srv2.URL, []string{"a"}, 10, nil)
	if _, err := c2.Fetch(context.Background()); err != nil || !c2.FetchComplete() {
		t.Fatalf("err=%v complete=%v, want complete", err, c2.FetchComplete())
	}
}

// Reviewer finding 2: a failed fetch after a complete one must not leave
// the old "complete" standing.
func TestFailedFetchResetsCompleteness(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"products":[{"id":1,"title":"D","handle":"d","product_type":"HDDs > 18TB","variants":[{"id":2,"price":"10.00","available":true}]}]}`))
	}))
	defer srv.Close()
	c := collConnector(srv.URL, []string{"a"}, 10, nil)
	if _, err := c.Fetch(context.Background()); err != nil || !c.FetchComplete() {
		t.Fatalf("err=%v complete=%v, want a complete first fetch", err, c.FetchComplete())
	}
	fail = true
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("want the 500 as an error")
	}
	if c.FetchComplete() {
		t.Fatal("a failed fetch must reset completeness")
	}
}

// The courtesy pause also separates one collection's last page from the
// next collection's first.
func TestCollectionsArePacedBetweenWalks(t *testing.T) {
	srv, _ := collectionStore(t, map[string][]int{"a": {1}, "b": {2}})
	var waited int
	c := NewConnector(Config{
		Name: "s", BaseURL: srv.URL, Collections: []string{"a", "b"}, Now: func() time.Time { return fixedNow },
		Sleep: func(context.Context, time.Duration) error { waited++; return nil },
	})
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if waited != 1 {
		t.Fatalf("waited %d times, want 1 (between the two single-page collections)", waited)
	}
}

// Every entry of Collections is validated before any request.
func TestCollectionsHandlesAreValidated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("made a request despite an invalid handle")
	}))
	defer srv.Close()
	c := collConnector(srv.URL, []string{"ok", "../admin"}, 10, nil)
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("want an error for an invalid handle")
	}
}
