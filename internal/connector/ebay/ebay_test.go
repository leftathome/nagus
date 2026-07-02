package ebay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testAccessToken = "test-access-token-123"

// testdataBytes reads testdata/browse_search.json, the fixture that doubles
// as both the offline fixture-mode input and the canned httptest search body.
func testdataBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/browse_search.json")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return data
}

// tokenOKHandler serves a well-formed OAuth2 client-credentials token
// response and increments callCount on every request, so tests can assert
// how many times the token endpoint was actually hit (caching behavior).
func tokenOKHandler(t *testing.T, callCount *int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(callCount, 1)
		if r.Method != http.MethodPost {
			t.Errorf("token request method = %s, want POST", r.Method)
		}
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Basic ") {
			t.Errorf("token request Authorization = %q, want Basic-prefixed", auth)
		}
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read token request body: %v", err)
		}
		body := string(bodyBytes)
		if !strings.Contains(body, "grant_type=client_credentials") {
			t.Errorf("token request body = %q, want grant_type=client_credentials", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oauthTokenResponse{
			AccessToken: testAccessToken,
			ExpiresIn:   7200,
			TokenType:   "Application Access Token",
		})
	}
}

// searchOKHandler asserts the expected auth/marketplace headers and query
// params, then serves body as the item_summary/search JSON response.
func searchOKHandler(t *testing.T, body []byte, wantMarketplace string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAccessToken {
			t.Errorf("search Authorization = %q, want Bearer %s", got, testAccessToken)
		}
		if got := r.Header.Get("X-EBAY-C-MARKETPLACE-ID"); got != wantMarketplace {
			t.Errorf("search X-EBAY-C-MARKETPLACE-ID = %q, want %q", got, wantMarketplace)
		}
		if got := r.URL.Query().Get("q"); got == "" {
			t.Errorf("search request missing q param")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
}

func TestFetch_OAuthAndSearch_HappyPath(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-token", tokenOKHandler(t, &tokenCalls))
	mux.HandleFunc("/buy/browse/v1/item_summary/search", searchOKHandler(t, testdataBytes(t), DefaultMarketplaceID))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		Query:        "internal hard drive",
		BaseURL:      srv.URL,
		OAuthURL:     srv.URL + "/oauth-token",
		HTTPClient:   srv.Client(),
		Now:          fixedNow,
	})

	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if len(raws) != 3 {
		t.Fatalf("len(raws) = %d, want 3", len(raws))
	}

	first := raws[0]
	if first.SourceID != "ebay" {
		t.Errorf("SourceID = %q, want ebay", first.SourceID)
	}
	if first.SourceKey != "v1|110599999901|0" {
		t.Errorf("SourceKey = %q, want v1|110599999901|0", first.SourceKey)
	}
	if first.SourceURL != "https://www.ebay.com/itm/110599999901" {
		t.Errorf("SourceURL = %q, want the item web url", first.SourceURL)
	}
	if first.Title != "Seagate Exos X18 16TB 7200RPM SATA 3.5in Enterprise Hard Drive ST18000NM000J" {
		t.Errorf("Title = %q, unexpected", first.Title)
	}
	if first.Body != "" {
		t.Errorf("Body = %q, want empty (search has no description)", first.Body)
	}
	if first.PriceCents != 27999 {
		t.Errorf("PriceCents = %d, want 27999 (from %q)", first.PriceCents, "279.99")
	}
	if first.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", first.Currency)
	}
	if first.ConditionRaw != "1000" {
		t.Errorf("ConditionRaw = %q, want 1000 (New)", first.ConditionRaw)
	}
	if first.Aspects["condition"] != "New" {
		t.Errorf(`Aspects["condition"] = %q, want "New"`, first.Aspects["condition"])
	}
	if first.Aspects["conditionId"] != "1000" {
		t.Errorf(`Aspects["conditionId"] = %q, want "1000"`, first.Aspects["conditionId"])
	}
	if !first.SeenAt.Equal(fixedNow()) {
		t.Errorf("SeenAt = %v, want %v", first.SeenAt, fixedNow())
	}

	second := raws[1]
	if second.PriceCents != 12999 {
		t.Errorf("second.PriceCents = %d, want 12999 (from %q)", second.PriceCents, "129.99")
	}
	if second.ConditionRaw != "2500" {
		t.Errorf("second.ConditionRaw = %q, want 2500 (Seller refurbished)", second.ConditionRaw)
	}

	third := raws[2]
	if third.PriceCents != 8950 {
		t.Errorf("third.PriceCents = %d, want 8950 (from %q, single fractional digit)", third.PriceCents, "89.5")
	}
	if third.ConditionRaw != "3000" {
		t.Errorf("third.ConditionRaw = %q, want 3000 (Used)", third.ConditionRaw)
	}

	if atomic.LoadInt32(&tokenCalls) != 1 {
		t.Errorf("token endpoint called %d times, want 1", tokenCalls)
	}
}

func TestFetch_TokenIsCachedAcrossFetches(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-token", tokenOKHandler(t, &tokenCalls))
	mux.HandleFunc("/buy/browse/v1/item_summary/search", searchOKHandler(t, testdataBytes(t), DefaultMarketplaceID))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		Query:        "internal hard drive",
		BaseURL:      srv.URL,
		OAuthURL:     srv.URL + "/oauth-token",
		HTTPClient:   srv.Client(),
		Now:          fixedNow,
	})

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}

	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Errorf("token endpoint called %d times across 2 Fetches, want 1 (cached)", got)
	}
}

func TestFetch_TokenRefreshedAfterExpiry(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-token", tokenOKHandler(t, &tokenCalls))
	mux.HandleFunc("/buy/browse/v1/item_summary/search", searchOKHandler(t, testdataBytes(t), DefaultMarketplaceID))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	now := fixedNow()
	c := NewConnector(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		Query:        "internal hard drive",
		BaseURL:      srv.URL,
		OAuthURL:     srv.URL + "/oauth-token",
		HTTPClient:   srv.Client(),
		Now:          func() time.Time { return now },
	})

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	// Advance past the token's 7200s lifetime (minus safety margin).
	now = now.Add(3 * time.Hour)
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}

	if got := atomic.LoadInt32(&tokenCalls); got != 2 {
		t.Errorf("token endpoint called %d times across an expiry boundary, want 2", got)
	}
}

func TestFetch_FixtureMode_NoNetworkCalls(t *testing.T) {
	failIfCalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected network call in fixture mode: %s %s", r.Method, r.URL)
	}))
	t.Cleanup(failIfCalled.Close)

	c := NewConnector(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		Query:        "internal hard drive",
		BaseURL:      failIfCalled.URL,
		OAuthURL:     failIfCalled.URL + "/oauth-token",
		HTTPClient:   failIfCalled.Client(),
		Now:          fixedNow,
		FixturePath:  "testdata/browse_search.json",
	})

	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if len(raws) != 3 {
		t.Fatalf("len(raws) = %d, want 3", len(raws))
	}
	if raws[0].SourceKey != "v1|110599999901|0" {
		t.Errorf("SourceKey = %q, want v1|110599999901|0", raws[0].SourceKey)
	}
	if raws[0].PriceCents != 27999 {
		t.Errorf("PriceCents = %d, want 27999", raws[0].PriceCents)
	}
	if !raws[0].SeenAt.Equal(fixedNow()) {
		t.Errorf("SeenAt = %v, want %v", raws[0].SeenAt, fixedNow())
	}
}

func TestFetch_SearchNon200_ReturnsError(t *testing.T) {
	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-token", tokenOKHandler(t, &tokenCalls))
	mux.HandleFunc("/buy/browse/v1/item_summary/search", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		Query:        "internal hard drive",
		BaseURL:      srv.URL,
		OAuthURL:     srv.URL + "/oauth-token",
		HTTPClient:   srv.Client(),
		Now:          fixedNow,
	})

	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 search response, got nil")
	}
}

func TestFetch_TokenNon200_ReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-token", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid client", http.StatusUnauthorized)
	})
	mux.HandleFunc("/buy/browse/v1/item_summary/search", searchOKHandler(t, testdataBytes(t), DefaultMarketplaceID))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		ClientID:     "id",
		ClientSecret: "wrong",
		Query:        "internal hard drive",
		BaseURL:      srv.URL,
		OAuthURL:     srv.URL + "/oauth-token",
		HTTPClient:   srv.Client(),
		Now:          fixedNow,
	})

	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 token response, got nil")
	}
}

func TestFetch_MissingItemID_IsSkipped(t *testing.T) {
	body := []byte(`{
		"total": 2,
		"itemSummaries": [
			{"itemId": "", "title": "no id, must be skipped", "itemWebUrl": "https://example.com/x"},
			{"itemId": "v1|999|0", "title": "has id, must be kept", "itemWebUrl": "https://example.com/y",
			 "condition": "Used", "conditionId": "3000", "price": {"value": "10.00", "currency": "USD"}}
		]
	}`)

	var tokenCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-token", tokenOKHandler(t, &tokenCalls))
	mux.HandleFunc("/buy/browse/v1/item_summary/search", searchOKHandler(t, body, DefaultMarketplaceID))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		Query:        "internal hard drive",
		BaseURL:      srv.URL,
		OAuthURL:     srv.URL + "/oauth-token",
		HTTPClient:   srv.Client(),
		Now:          fixedNow,
	})

	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if len(raws) != 1 {
		t.Fatalf("len(raws) = %d, want 1 (missing-itemId item skipped)", len(raws))
	}
	if raws[0].SourceKey != "v1|999|0" {
		t.Errorf("SourceKey = %q, want v1|999|0", raws[0].SourceKey)
	}
}

func TestSourceID(t *testing.T) {
	c := NewConnector(Config{ClientID: "id", ClientSecret: "secret"})
	if got := c.SourceID(); got != "ebay" {
		t.Errorf("SourceID() = %q, want ebay", got)
	}
}

func TestParsePriceToCents(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"129.99", 12999, false},
		{"89.5", 8950, false},
		{"10", 1000, false},
		{"0.01", 1, false},
		{"279.99", 27999, false},
		{"1999.999", 199999, false}, // truncate beyond 2 fractional digits
		{"", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := parsePriceToCents(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parsePriceToCents(%q): expected error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePriceToCents(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parsePriceToCents(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestNewConnector_Defaults(t *testing.T) {
	c := NewConnector(Config{ClientID: "id", ClientSecret: "secret"})
	if c.cfg.MarketplaceID != DefaultMarketplaceID {
		t.Errorf("MarketplaceID = %q, want %q", c.cfg.MarketplaceID, DefaultMarketplaceID)
	}
	if c.cfg.Limit != DefaultLimit {
		t.Errorf("Limit = %d, want %d", c.cfg.Limit, DefaultLimit)
	}
	if c.cfg.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.cfg.BaseURL, DefaultBaseURL)
	}
	if c.cfg.OAuthURL != DefaultOAuthURL {
		t.Errorf("OAuthURL = %q, want %q", c.cfg.OAuthURL, DefaultOAuthURL)
	}
	if c.cfg.HTTPClient != http.DefaultClient {
		t.Errorf("HTTPClient = %v, want http.DefaultClient", c.cfg.HTTPClient)
	}
	if c.cfg.Now == nil {
		t.Error("Now should default to a non-nil func")
	}
}
