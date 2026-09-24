package sanitize

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
	g, err := NewGate(srv.URL, "tok-123", "", srv.Client(), t.Logf)
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
	for _, code := range []int{400, 413, 500} {
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
	g, err := NewGate("http://127.0.0.1:1", "tok", "", &http.Client{Timeout: 500 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Sanitize(context.Background(), raw); err == nil {
		t.Fatal("an unreachable glovebox must drop, not pass")
	}
}

func TestGateNeedsURLAndToken(t *testing.T) {
	if _, err := NewGate("", "tok", "", nil, nil); err == nil {
		t.Fatal("no URL must be an error")
	}
	if _, err := NewGate("http://x", "", "", nil, nil); err == nil {
		t.Fatal("no token must be an error")
	}
}

// A rejected token is systemic: the first 401 trips the gate, every listing
// after it is dropped WITHOUT a call (no brute-force storm against glovebox),
// and after the cooldown one call probes again (nagus-lg7).
func TestRejectedTokenTripsTheGate(t *testing.T) {
	f := &fakeGlovebox{statuses: []int{401}, verdict: "pass"}
	srv := f.server(t)
	defer srv.Close()
	g := gate(t, srv)
	clock := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	g.Now = func() time.Time { return clock }
	g.Cooldown = 5 * time.Minute

	if _, err := g.Sanitize(context.Background(), raw); err == nil {
		t.Fatal("401 must drop")
	}
	for i := 0; i < 50; i++ {
		if _, err := g.Sanitize(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "tripped") {
			t.Fatalf("a tripped gate must drop without calling: %v", err)
		}
	}
	if f.calls != 1 {
		t.Fatalf("glovebox was called %d times; a tripped gate must not call it", f.calls)
	}
	// After the cooldown one probe goes out; the fake now answers pass.
	clock = clock.Add(6 * time.Minute)
	if _, err := g.Sanitize(context.Background(), raw); err != nil {
		t.Fatalf("after the cooldown the gate must probe and recover: %v", err)
	}
	st := g.Snapshot()
	if st.Unauthorized != 1 || st.Tripped != 50 || st.Pass != 1 || f.calls != 2 {
		t.Fatalf("stats %+v calls %d", st, f.calls)
	}
}

// A token FILE that is empty at start (its Secret synced after the pod
// started) heals without a restart once it is written (nagus-4g3).
func TestLateSyncedTokenFileHealsWithoutARestart(t *testing.T) {
	f := &fakeGlovebox{verdict: "pass"}
	srv := f.server(t)
	defer srv.Close()
	file := t.TempDir() + "/NAGUS_GLOVEBOX_TOKEN"
	g, err := NewGate(srv.URL, "", file, srv.Client(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Sanitize(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "no token yet") {
		t.Fatalf("no token file yet must drop and say why: %v", err)
	}
	if f.calls != 0 {
		t.Fatal("no call may go out without a token")
	}
	if err := os.WriteFile(file, []byte("tok-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Sanitize(context.Background(), raw); err != nil {
		t.Fatalf("once the file is written the gate must work without a restart: %v", err)
	}
	if f.gotAuth != "Bearer tok-from-file" {
		t.Fatalf("auth %q (the file value, trimmed)", f.gotAuth)
	}
	if st := g.Snapshot(); st.NoToken != 1 || st.Pass != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// A trip re-reads the token file, so a rotated token heals at the next probe.
func TestTripRereadsTheTokenFile(t *testing.T) {
	f := &fakeGlovebox{statuses: []int{401}, verdict: "pass"}
	srv := f.server(t)
	defer srv.Close()
	file := t.TempDir() + "/tok"
	if err := os.WriteFile(file, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := NewGate(srv.URL, "", file, srv.Client(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	g.Now = func() time.Time { return clock }
	_, _ = g.Sanitize(context.Background(), raw) // 401 with "old": trips
	if err := os.WriteFile(file, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Minute)
	if _, err := g.Sanitize(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if f.gotAuth != "Bearer new" {
		t.Fatalf("the probe must use the re-read token, sent %q", f.gotAuth)
	}
}
