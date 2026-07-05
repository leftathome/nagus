package ebay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newShoppingServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("callname"); got != "GetUserProfile" {
			t.Errorf("callname = %q, want GetUserProfile", got)
		}
		if got := r.URL.Query().Get("UserID"); got == "" {
			t.Errorf("UserID query param missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestShoppingProfileSource_ParsesAgeAndRecentSales(t *testing.T) {
	body := `{"Ack":"Success","User":{
		"RegistrationDate":"2020-01-01T00:00:00.000Z",
		"FeedbackHistory":{"PositiveFeedbackPeriodArray":{"FeedbackPeriod":[
			{"PeriodInDays":30,"Count":45},{"PeriodInDays":365,"Count":300}]}}}}`
	srv := newShoppingServer(t, http.StatusOK, body)

	src := &ShoppingProfileSource{
		AppID: "id", BaseURL: srv.URL, HTTPClient: srv.Client(),
		Now: func() time.Time { return time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	p, found, err := src.Profile(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if !p.HasAccountAge || p.AccountAgeDays != 366 { // 2020 is a leap year
		t.Fatalf("AccountAgeDays = %d (has=%v), want 366", p.AccountAgeDays, p.HasAccountAge)
	}
	if !p.HasRecentSales || p.RecentSales != 45 {
		t.Fatalf("RecentSales = %d (has=%v), want 45 (30-day window)", p.RecentSales, p.HasRecentSales)
	}
}

func TestShoppingProfileSource_FailureAckNotFound(t *testing.T) {
	srv := newShoppingServer(t, http.StatusOK, `{"Ack":"Failure","Errors":[{"ShortMessage":"nope"}]}`)
	src := &ShoppingProfileSource{AppID: "id", BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, found, err := src.Profile(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if found {
		t.Fatal("found = true, want false for a non-Success Ack")
	}
}

func TestShoppingProfileSource_PartialProfileOmitsMissingField(t *testing.T) {
	// Registration date present, feedback history absent -> age resolved, recent not.
	srv := newShoppingServer(t, http.StatusOK, `{"Ack":"Success","User":{"RegistrationDate":"2019-06-01T00:00:00.000Z"}}`)
	src := &ShoppingProfileSource{
		AppID: "id", BaseURL: srv.URL, HTTPClient: srv.Client(),
		Now: func() time.Time { return time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	p, found, err := src.Profile(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if !found || !p.HasAccountAge {
		t.Fatalf("expected found with account age, got found=%v hasAge=%v", found, p.HasAccountAge)
	}
	if p.HasRecentSales {
		t.Fatalf("HasRecentSales = true, want false (no feedback history)")
	}
}

func TestNewShoppingProfileSource_SelectsHost(t *testing.T) {
	if s := NewShoppingProfileSource("id", false, nil); s.BaseURL != DefaultShoppingBaseURL {
		t.Fatalf("prod BaseURL = %q, want %q", s.BaseURL, DefaultShoppingBaseURL)
	}
	if s := NewShoppingProfileSource("id", true, nil); s.BaseURL != DefaultShoppingSandboxBaseURL {
		t.Fatalf("sandbox BaseURL = %q, want %q", s.BaseURL, DefaultShoppingSandboxBaseURL)
	}
}
