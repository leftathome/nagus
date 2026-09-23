package sanitize

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/listing"
)

// fakeGlovebox answers /v1/sanitize with the scripted statuses, then with a
// verdict. It records what it was sent.
type fakeGlovebox struct {
	statuses []int  // served in order before the verdict
	verdict  string // "pass" or "quarantine"
	calls    int
	gotAuth  string
	gotBody  map[string]any
}

func (f *fakeGlovebox) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sanitize" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		f.calls++
		f.gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&f.gotBody)
		if len(f.statuses) > 0 {
			code := f.statuses[0]
			f.statuses = f.statuses[1:]
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		sigs := `[]`
		if f.verdict == "quarantine" {
			sigs = `[{"name":"ignore-previous","weight":0.9,"matched":"ignore previous instructions"}]`
		}
		_, _ = w.Write([]byte(`{"verdict":"` + f.verdict + `","total_score":0.9,"signals":` + sigs + `}`))
	}))
}

func gate(t *testing.T, srv *httptest.Server) *Gate {
	t.Helper()
	g, err := NewGate(srv.URL, "tok-123", srv.Client(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	g.Sleep = func(context.Context, time.Duration) error { return nil }
	return g
}

var raw = listing.Raw{SourceID: "shopify:x", SourceKey: "1", Title: "2021 Syrah", Body: "Dark fruit.",
	PriceCents: 4000, Aspects: map[string]string{"vendor": "Kiona", "ship_legal_to": "US-WA"}}

func TestPassKeepsTheOriginalBytes(t *testing.T) {
	f := &fakeGlovebox{verdict: "pass"}
	srv := f.server(t)
	defer srv.Close()
	s, err := gate(t, srv).Sanitize(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != raw.Title || s.Body != raw.Body || s.PriceCents != 4000 || s.Aspects["vendor"] != "Kiona" {
		t.Fatalf("a passed listing must keep its original bytes: %+v", s)
	}
	if s.Boundary != "glovebox.sanitize" {
		t.Fatalf("boundary %q", s.Boundary)
	}
	if f.gotAuth != "Bearer tok-123" {
		t.Fatalf("auth header %q", f.gotAuth)
	}
	content, _ := f.gotBody["content"].(string)
	for _, want := range []string{"2021 Syrah", "Dark fruit.", "Kiona"} {
		if !strings.Contains(content, want) {
			t.Fatalf("classified content must include %q: %q", want, content)
		}
	}
}

func TestQuarantineDrops(t *testing.T) {
	f := &fakeGlovebox{verdict: "quarantine"}
	srv := f.server(t)
	defer srv.Close()
	_, err := gate(t, srv).Sanitize(context.Background(), raw)
	if !errors.Is(err, ErrQuarantined) || !strings.Contains(err.Error(), "ignore-previous") {
		t.Fatalf("quarantine must drop and name the signal: %v", err)
	}
}

// Every non-2xx is a drop: the gate never fails open.
func TestErrorStatusesDrop(t *testing.T) {
	for _, code := range []int{400, 401, 413, 500} {
		f := &fakeGlovebox{statuses: []int{code}, verdict: "pass"}
		srv := f.server(t)
		_, err := gate(t, srv).Sanitize(context.Background(), raw)
		srv.Close()
		if err == nil {
			t.Fatalf("HTTP %d must drop, not pass", code)
		}
		if f.calls != 1 {
			t.Fatalf("HTTP %d is not transient and must not be retried (calls %d)", code, f.calls)
		}
	}
}

// 429 and 503 are transient: retried, and a verdict after them counts.
func TestTransientStatusesAreRetried(t *testing.T) {
	f := &fakeGlovebox{statuses: []int{429, 503}, verdict: "pass"}
	srv := f.server(t)
	defer srv.Close()
	if _, err := gate(t, srv).Sanitize(context.Background(), raw); err != nil {
		t.Fatalf("a verdict after transient errors must count: %v", err)
	}
	if f.calls != 3 {
		t.Fatalf("calls %d, want 3", f.calls)
	}
}

// Retries that never reach a verdict still drop.
func TestTransientRetriesExhaustedDrop(t *testing.T) {
	f := &fakeGlovebox{statuses: []int{503, 503, 503, 503, 503}, verdict: "pass"}
	srv := f.server(t)
	defer srv.Close()
	if _, err := gate(t, srv).Sanitize(context.Background(), raw); err == nil {
		t.Fatal("exhausted retries must drop, not pass")
	}
}

func TestUnreachableDrops(t *testing.T) {
	g, err := NewGate("http://127.0.0.1:1", "tok", &http.Client{Timeout: 500 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Sanitize(context.Background(), raw); err == nil {
		t.Fatal("an unreachable glovebox must drop, not pass")
	}
}

func TestGateNeedsURLAndToken(t *testing.T) {
	if _, err := NewGate("", "tok", nil, nil); err == nil {
		t.Fatal("no URL must be an error")
	}
	if _, err := NewGate("http://x", "", nil, nil); err == nil {
		t.Fatal("no token must be an error")
	}
}
