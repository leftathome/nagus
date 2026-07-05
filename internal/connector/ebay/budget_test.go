package ebay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallBudget_ReserveUntilExhausted(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC) }
	b := newCallBudget(3, now)
	for i := 0; i < 3; i++ {
		if err := b.reserve(); err != nil {
			t.Fatalf("reserve #%d = %v, want nil (within budget)", i+1, err)
		}
	}
	if err := b.reserve(); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("reserve past budget = %v, want ErrBudgetExhausted", err)
	}
}

func TestCallBudget_DayRolloverResets(t *testing.T) {
	cur := time.Date(2026, 7, 5, 23, 0, 0, 0, time.UTC)
	b := newCallBudget(2, func() time.Time { return cur })
	_ = b.reserve()
	_ = b.reserve()
	if err := b.reserve(); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("reserve past budget = %v, want ErrBudgetExhausted", err)
	}
	// Cross UTC midnight: the daily counter must reset.
	cur = cur.Add(2 * time.Hour) // -> 2026-07-06 01:00 UTC
	if err := b.reserve(); err != nil {
		t.Fatalf("reserve after day rollover = %v, want nil (counter reset)", err)
	}
}

func TestCallBudget_HonorsHeaderRemaining(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC) }

	// An eBay-reported remaining count caps calls BELOW the local budget.
	b := newCallBudget(100, now)
	b.observeRemaining(1)
	if err := b.reserve(); err != nil {
		t.Fatalf("reserve with header remaining=1 = %v, want nil", err)
	}
	if err := b.reserve(); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("reserve after header remaining exhausted = %v, want ErrBudgetExhausted", err)
	}

	// A reported remaining of 0 blocks immediately, even with local headroom.
	b2 := newCallBudget(100, now)
	b2.observeRemaining(0)
	if err := b2.reserve(); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("reserve with header remaining=0 = %v, want ErrBudgetExhausted", err)
	}
}

func TestCallBudget_StatsReflectsUsedAndHeader(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC) }
	b := newCallBudget(10, now)
	_ = b.reserve()
	_ = b.reserve()
	if s := b.stats(); s.Budget != 10 || s.Used != 2 || s.Remaining != 8 {
		t.Fatalf("stats = %+v, want {Budget:10 Used:2 Remaining:8}", s)
	}
	// A tighter header-reported remaining wins the min.
	b.observeRemaining(3)
	if s := b.stats(); s.Remaining != 3 {
		t.Fatalf("stats.Remaining = %d, want 3 (header caps below local)", s.Remaining)
	}
}

func TestNewCallBudget_DefaultsWhenNonPositive(t *testing.T) {
	b := newCallBudget(0, func() time.Time { return time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC) })
	if s := b.stats(); s.Budget != DefaultDailyBudget {
		t.Fatalf("stats.Budget = %d, want default %d", s.Budget, DefaultDailyBudget)
	}
}

// TestFetch_BudgetExhausted_StopsSearchCalls verifies the connector stops making
// API calls once the daily budget is spent -- it does not (and must not) find
// another way to reach eBay. With budget=2, the first Fetch spends token+search;
// the second Fetch (token cached) is blocked before the search call.
func TestFetch_BudgetExhausted_StopsSearchCalls(t *testing.T) {
	var tokenCalls, searchCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-token", tokenOKHandler(t, &tokenCalls))
	mux.HandleFunc("/buy/browse/v1/item_summary/search", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&searchCalls, 1)
		searchOKHandler(t, testdataBytes(t), DefaultMarketplaceID)(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		ClientID: "id", ClientSecret: "secret", Query: "hdd",
		BaseURL: srv.URL, OAuthURL: srv.URL + "/oauth-token",
		HTTPClient: srv.Client(), Now: fixedNow,
		DailyBudget: 2,
	})

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch = %v, want success", err)
	}
	_, err := c.Fetch(context.Background())
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second Fetch = %v, want ErrBudgetExhausted", err)
	}
	if got := atomic.LoadInt32(&searchCalls); got != 1 {
		t.Fatalf("search endpoint called %d times, want 1 (second call must be blocked)", got)
	}
}

// TestFetch_HonorsRateLimitRemainingHeader verifies that when eBay reports zero
// calls remaining in a response header, the next Fetch is blocked even though
// the local daily budget still has headroom.
func TestFetch_HonorsRateLimitRemainingHeader(t *testing.T) {
	var searchCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-token", tokenOKHandler(t, new(int32)))
	mux.HandleFunc("/buy/browse/v1/item_summary/search", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&searchCalls, 1)
		w.Header().Set(rateLimitRemainingHeader, "0")
		searchOKHandler(t, testdataBytes(t), DefaultMarketplaceID)(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		ClientID: "id", ClientSecret: "secret", Query: "hdd",
		BaseURL: srv.URL, OAuthURL: srv.URL + "/oauth-token",
		HTTPClient: srv.Client(), Now: fixedNow,
		DailyBudget: 1000, // plenty of local headroom; the header must still win
	})

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch = %v, want success", err)
	}
	if _, err := c.Fetch(context.Background()); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second Fetch = %v, want ErrBudgetExhausted (header remaining=0)", err)
	}
	if got := atomic.LoadInt32(&searchCalls); got != 1 {
		t.Fatalf("search endpoint called %d times, want 1", got)
	}
}
