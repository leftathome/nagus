package ebay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// var _ ... is a compile-time assertion that Connector satisfies the
// FetchComplete contract the pipeline's ingester type-asserts for (see
// internal/pipeline/ingester.go), mirroring the Shopify connector.
var _ interface{ FetchComplete() bool } = (*Connector)(nil)

// newFetchCompleteConnector builds a Connector wired to an httptest.Server
// that serves tokenOKHandler for OAuth and body for the Browse search call,
// reusing the fakes already established in ebay_test.go.
func newFetchCompleteConnector(t *testing.T, body []byte) *Connector {
	t.Helper()
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-token", tokenOKHandler(t, &tokenCalls))
	mux.HandleFunc("/buy/browse/v1/item_summary/search", searchOKHandler(t, body, DefaultMarketplaceID))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return NewConnector(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		Query:        "internal hard drive",
		BaseURL:      srv.URL,
		OAuthURL:     srv.URL + "/oauth-token",
		HTTPClient:   srv.Client(),
		Now:          fixedNow,
	})
}

// TestFetchComplete_DefaultBeforeFetch asserts the pre-Fetch default matches
// Shopify's: the zero value of the tracking bool, i.e. false ("not yet known
// to be complete") rather than optimistically true.
func TestFetchComplete_DefaultBeforeFetch(t *testing.T) {
	c := NewConnector(Config{ClientID: "id", ClientSecret: "secret", Query: "q"})
	if got := c.FetchComplete(); got != false {
		t.Errorf("FetchComplete() before any Fetch = %v, want false (Shopify's default)", got)
	}
}

// TestFetchComplete_PartialPage covers the hazard this method exists to
// prevent: eBay reporting more matching items (total) than this single,
// Limit-capped page returned.
func TestFetchComplete_PartialPage(t *testing.T) {
	body := []byte(`{"total":5,"itemSummaries":[{"itemId":"v1|1|0"},{"itemId":"v1|2|0"}]}`)
	c := newFetchCompleteConnector(t, body)

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if got := c.FetchComplete(); got != false {
		t.Errorf("FetchComplete() after partial page (total=5, returned=2) = %v, want false", got)
	}
}

// TestFetchComplete_FullPage covers total == returned: the page holds every
// item eBay reported.
func TestFetchComplete_FullPage(t *testing.T) {
	body := []byte(`{"total":2,"itemSummaries":[{"itemId":"v1|1|0"},{"itemId":"v1|2|0"}]}`)
	c := newFetchCompleteConnector(t, body)

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if got := c.FetchComplete(); got != true {
		t.Errorf("FetchComplete() after full page (total=2, returned=2) = %v, want true", got)
	}
}

// TestFetchComplete_NoTotalField covers a response that omits "total"
// entirely (decodes to the int zero value): treated as complete, preserving
// the connector's prior behavior of trusting whatever came back when eBay
// gives no bound to compare against.
func TestFetchComplete_NoTotalField(t *testing.T) {
	body := []byte(`{"itemSummaries":[{"itemId":"v1|1|0"}]}`)
	c := newFetchCompleteConnector(t, body)

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if got := c.FetchComplete(); got != true {
		t.Errorf("FetchComplete() with no total field = %v, want true", got)
	}
}

// TestFetchComplete_FlipsAcrossFetches asserts the tracked value reflects
// only the MOST RECENT Fetch, flipping from true to false and back as
// successive pages differ -- guarding against a stale "complete" reading
// left over from an earlier, fuller page.
func TestFetchComplete_FlipsAcrossFetches(t *testing.T) {
	full := []byte(`{"total":1,"itemSummaries":[{"itemId":"v1|1|0"}]}`)
	c := newFetchCompleteConnector(t, full)
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch (full) returned error: %v", err)
	}
	if got := c.FetchComplete(); got != true {
		t.Fatalf("FetchComplete() after full page = %v, want true", got)
	}

	partial := []byte(`{"total":9,"itemSummaries":[{"itemId":"v1|1|0"}]}`)
	c2 := newFetchCompleteConnector(t, partial)
	if _, err := c2.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch (partial) returned error: %v", err)
	}
	if got := c2.FetchComplete(); got != false {
		t.Fatalf("FetchComplete() after partial page = %v, want false", got)
	}
}
