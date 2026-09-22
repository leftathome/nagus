package webfetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetHonoursRetryAfterThenSucceeds(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" || r.Header.Get("tenant") != "x" {
			t.Errorf("headers not sent: %v", r.Header)
		}
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	var waited time.Duration
	c := &Client{HTTP: srv.Client(), Sleep: func(_ context.Context, d time.Duration) error { waited = d; return nil }}
	body, err := c.Get(context.Background(), srv.URL, map[string]string{"tenant": "x"})
	if err != nil || string(body) != "ok" {
		t.Fatalf("Get: %q %v", body, err)
	}
	if waited != 7*time.Second {
		t.Fatalf("waited %s, want the server's 7s", waited)
	}
}

func TestGetGivesUpOnPersistent429AndCapsTheWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "86400")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	var max time.Duration
	c := &Client{HTTP: srv.Client(), MaxRetries: 2, MaxWait: time.Minute,
		Sleep: func(_ context.Context, d time.Duration) error {
			if d > max {
				max = d
			}
			return nil
		}}
	_, err := c.Get(context.Background(), srv.URL, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if max != time.Minute {
		t.Fatalf("longest wait %s, want the 1m cap (a day-long Retry-After must not wedge ingest)", max)
	}
}

func TestGetNon200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := (&Client{HTTP: srv.Client()}).Get(context.Background(), srv.URL, nil); err == nil {
		t.Fatal("401 must be an error")
	}
}
