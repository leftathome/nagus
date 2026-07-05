package ebay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeProfileSource records the usernames it is asked about and returns a fixed
// profile, so tests can assert the username is used transiently (passed here)
// and never persisted, and that per-fetch caching dedupes calls.
type fakeProfileSource struct {
	mu        sync.Mutex
	calls     []string
	lastToken string
	prof      SellerProfile
	found     bool
}

func (f *fakeProfileSource) Profile(_ context.Context, username, token string) (SellerProfile, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, username)
	f.lastToken = token
	return f.prof, f.found, nil
}

func (f *fakeProfileSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// twoItemsSameSeller is a Browse response with two listings from one seller
// ("acme"), for exercising the per-fetch profile cache.
const twoItemsSameSeller = `{"itemSummaries":[
	{"itemId":"v1|1|0","title":"Seagate 16TB","itemWebUrl":"https://ex/1","conditionId":"2500","price":{"value":"100.00","currency":"USD"},"seller":{"username":"acme","feedbackPercentage":"99.5","feedbackScore":1000},"itemLocation":{"country":"US"}},
	{"itemId":"v1|2|0","title":"WD 12TB","itemWebUrl":"https://ex/2","conditionId":"2500","price":{"value":"90.00","currency":"USD"},"seller":{"username":"acme","feedbackPercentage":"99.5","feedbackScore":1000},"itemLocation":{"country":"US"}}
]}`

func newProfileConnector(t *testing.T, body string, src SellerProfileSource, budget int) *Connector {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-token", tokenOKHandler(t, new(int32)))
	mux.HandleFunc("/buy/browse/v1/item_summary/search", searchOKHandler(t, []byte(body), DefaultMarketplaceID))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewConnector(Config{
		ClientID: "id", ClientSecret: "secret", Query: "hdd",
		BaseURL: srv.URL, OAuthURL: srv.URL + "/oauth-token",
		HTTPClient: srv.Client(), Now: fixedNow,
		SellerProfile: src, DailyBudget: budget,
	})
}

func TestFetch_EnrichesSellerProfile_UsernameTransient(t *testing.T) {
	src := &fakeProfileSource{prof: SellerProfile{AccountAgeDays: 800, HasAccountAge: true, RecentSales: 42, HasRecentSales: true}, found: true}
	c := newProfileConnector(t, twoItemsSameSeller, src, 1000)

	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(raws) != 2 {
		t.Fatalf("len(raws) = %d, want 2", len(raws))
	}
	for _, r := range raws {
		if got := r.Aspects["seller_account_age_days"]; got != "800" {
			t.Errorf("seller_account_age_days = %q, want 800", got)
		}
		if got := r.Aspects["seller_recent_sales"]; got != "42" {
			t.Errorf("seller_recent_sales = %q, want 42", got)
		}
		// The username must be used only as a transient API argument, never emitted.
		for k, v := range r.Aspects {
			if v == "acme" || k == "seller_username" {
				t.Errorf("username leaked into Aspects[%q] = %q", k, v)
			}
		}
	}
	// The source WAS asked about the real username (transiently).
	if src.callCount() == 0 || src.calls[0] != "acme" {
		t.Fatalf("profile source calls = %v, want it queried for \"acme\"", src.calls)
	}
	// It also received the connector's Browse OAuth token to auth the lookup.
	if src.lastToken != testAccessToken {
		t.Fatalf("profile source got token %q, want the Browse token %q", src.lastToken, testAccessToken)
	}
}

func TestFetch_SellerProfileCachedPerFetch(t *testing.T) {
	src := &fakeProfileSource{prof: SellerProfile{AccountAgeDays: 800, HasAccountAge: true, RecentSales: 42, HasRecentSales: true}, found: true}
	c := newProfileConnector(t, twoItemsSameSeller, src, 1000)

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Two listings, one seller -> the profile is fetched exactly once per fetch.
	if n := src.callCount(); n != 1 {
		t.Fatalf("profile source called %d times, want 1 (per-fetch cache dedup)", n)
	}
}

func TestFetch_SellerProfileBudgetedDegradesGracefully(t *testing.T) {
	src := &fakeProfileSource{prof: SellerProfile{AccountAgeDays: 800, HasAccountAge: true, RecentSales: 42, HasRecentSales: true}, found: true}
	// Budget of 2 is spent by the token + search calls, leaving nothing for the
	// profile lookup: enrichment is skipped but the base listings still return.
	c := newProfileConnector(t, twoItemsSameSeller, src, 2)

	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(raws) != 2 {
		t.Fatalf("len(raws) = %d, want 2 (listings survive even when profile budget is spent)", len(raws))
	}
	if _, ok := raws[0].Aspects["seller_account_age_days"]; ok {
		t.Fatalf("expected no profile enrichment once budget is exhausted")
	}
}
