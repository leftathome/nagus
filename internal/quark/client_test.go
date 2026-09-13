package quark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const token = "t0000000000000000000000000000000000000000000000000000000000000000"

// fakeQuark enforces the parts of quark's contract nagus depends on: bearer
// auth, a CLOSED request schema (unknown members are a 422, exactly as the
// deployed service does), and positional results.
func fakeQuark(t *testing.T, respond func(hints []Hint) (int, any)) (*httptest.Server, *[]map[string]json.RawMessage) {
	t.Helper()
	var seen []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/resolve" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"title":"Unauthenticated","status":401}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seen = append(seen, body)
		for k := range body {
			if k != "hints" && k != "retry" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
		}
		var hints []Hint
		_ = json.Unmarshal(body["hints"], &hints)
		code, out := respond(hints)
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func minted(hints []Hint) (int, any) {
	res := make([]Result, len(hints))
	for i, h := range hints {
		if h.MPN == "" {
			res[i] = Result{Route: RouteRefused, Reason: "unidentifiable"}
			continue
		}
		res[i] = Result{ProductID: "p-" + h.MPN, Route: RouteMinted, Confidence: 100, Standing: "provisional"}
	}
	return http.StatusOK, map[string]any{"results": res}
}

func TestResolveMapsResultsPositionally(t *testing.T) {
	srv, _ := fakeQuark(t, minted)
	c := &Client{BaseURL: srv.URL + "/", Token: token}
	got, err := c.Resolve(context.Background(), []Hint{
		{Category: "hdd", Brand: "Seagate", MPN: "ST1"},
		{Category: "hdd", Brand: "Water Panther"},
	}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Results[0].ProductID != "p-ST1" || got.Results[1].Route != RouteRefused {
		t.Fatalf("results = %+v", got.Results)
	}
	if got.CatalogGeneration != 0 {
		t.Fatalf("a quark that reports no generation must read as 0, got %d", got.CatalogGeneration)
	}
}

func TestResolveOmitsRetryUnlessTrue(t *testing.T) {
	// The deployed quark rejects unknown members with 422, so sending
	// "retry":false to it would break every call. Omitted unless true.
	srv, seen := fakeQuark(t, minted)
	c := &Client{BaseURL: srv.URL, Token: token}
	if _, err := c.Resolve(context.Background(), []Hint{{Category: "hdd", MPN: "ST1"}}, false); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, has := (*seen)[0]["retry"]; has {
		t.Fatal("retry was sent on a non-retry batch")
	}
	if _, err := c.Resolve(context.Background(), []Hint{{Category: "hdd", MPN: "ST1"}}, true); err != nil {
		t.Fatalf("Resolve(retry): %v", err)
	}
	if string((*seen)[1]["retry"]) != "true" {
		t.Fatalf("retry batch sent retry=%s", (*seen)[1]["retry"])
	}
}

func TestResolveOmitsEmptyHintFields(t *testing.T) {
	srv, seen := fakeQuark(t, minted)
	c := &Client{BaseURL: srv.URL, Token: token}
	if _, err := c.Resolve(context.Background(), []Hint{{Category: "hdd", Brand: "Seagate"}}, false); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if strings.Contains(string((*seen)[0]["hints"]), `"mpn"`) {
		t.Fatalf("empty fields were sent: %s", (*seen)[0]["hints"])
	}
}

func TestResolveUnauthorized(t *testing.T) {
	srv, _ := fakeQuark(t, minted)
	c := &Client{BaseURL: srv.URL, Token: "wrong"}
	_, err := c.Resolve(context.Background(), []Hint{{Category: "hdd", MPN: "ST1"}}, false)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatal("the token appeared in the error")
	}
}

func TestResolveRejectsMisalignedResults(t *testing.T) {
	srv, _ := fakeQuark(t, func(hints []Hint) (int, any) {
		return http.StatusOK, map[string]any{"results": []Result{{Route: RouteMinted, ProductID: "p"}}}
	})
	c := &Client{BaseURL: srv.URL, Token: token}
	if _, err := c.Resolve(context.Background(), []Hint{{MPN: "a"}, {MPN: "b"}}, false); err == nil {
		t.Fatal("1 result for 2 hints must be an error: it cannot be mapped back to offers")
	}
}

func TestResolveServerErrorIsAnError(t *testing.T) {
	srv, _ := fakeQuark(t, func([]Hint) (int, any) {
		return http.StatusServiceUnavailable, map[string]any{"detail": "the product store is unavailable"}
	})
	c := &Client{BaseURL: srv.URL, Token: token}
	_, err := c.Resolve(context.Background(), []Hint{{Category: "hdd", MPN: "ST1"}}, false)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want a 503 error", err)
	}
}

func TestResolveRefusesOversizeBatchLocally(t *testing.T) {
	c := &Client{BaseURL: "http://unused.invalid", Token: token}
	if _, err := c.Resolve(context.Background(), make([]Hint, MaxBatch+1), false); err == nil {
		t.Fatal("a 501-hint batch must be refused before it is sent")
	}
}

func TestResolveReadsCatalogGeneration(t *testing.T) {
	srv, _ := fakeQuark(t, func(hints []Hint) (int, any) {
		_, body := minted(hints)
		m := body.(map[string]any)
		m["catalog_generation"] = 7
		return http.StatusOK, m
	})
	c := &Client{BaseURL: srv.URL, Token: token}
	got, err := c.Resolve(context.Background(), []Hint{{Category: "hdd", MPN: "ST1"}}, false)
	if err != nil || got.CatalogGeneration != 7 {
		t.Fatalf("generation = %d err = %v, want 7", got.CatalogGeneration, err)
	}
}
