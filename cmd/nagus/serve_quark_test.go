package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/watch"
)

func searchRows(t *testing.T, srv *server, path string) []searchRow {
	t.Helper()
	rec := do(t, srv, http.MethodGet, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, rec.Code)
	}
	var body struct {
		Items []searchRow `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return body.Items
}

// offerFor puts the offer that ingest would have written for a surfaced item:
// same source identity, hence the same id.
func offerFor(t *testing.T, srv *server, offers offer.Store, itemID, mpn string) offer.Offer {
	t.Helper()
	rec := do(t, srv, http.MethodGet, "/item?id="+itemID)
	var it struct {
		SourceID  string `json:"source_id"`
		SourceKey string `json:"source_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &it); err != nil || it.SourceID == "" {
		t.Fatalf("item %s: %v %+v", itemID, err, it)
	}
	o := offer.Offer{SourceID: it.SourceID, SourceKey: it.SourceKey, PriceCents: 100,
		LastSeen: time.Now(), ProductHint: offer.ProductHint{Brand: "Seagate", MPN: mpn}}
	if err := offers.Put(context.Background(), o); err != nil {
		t.Fatalf("Put: %v", err)
	}
	o.ID = offer.DeterministicID(it.SourceID, it.SourceKey)
	if o.ID != itemID {
		t.Fatalf("offer id %s != item id %s: the lookup withProductIDs relies on is broken", o.ID, itemID)
	}
	return o
}

// Rows carry quark's product id when -- and only when -- quark resolved the
// listing's offer. This is what lets a consumer see one product at several
// sellers.
func TestSearchRowsCarryQuarkProductID(t *testing.T) {
	srv := newTestServer(t)
	offers := offer.NewMemoryStore()
	srv.offers = offers

	rows := searchRows(t, srv, "/search?category=hdd")
	if len(rows) < 2 {
		t.Fatalf("fixture surfaced %d rows, need 2", len(rows))
	}
	resolved := offerFor(t, srv, offers, rows[0].ID, "ST1")
	offerFor(t, srv, offers, rows[1].ID, "ST2") // stays unattempted
	applied, err := offers.RecordResolution(context.Background(), resolved.ID, resolved.ProductHint.Fingerprint(),
		offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-ST1", Generation: 1, At: time.Now()})
	if err != nil || !applied {
		t.Fatalf("RecordResolution: %v %v", applied, err)
	}

	byID := map[string]string{}
	for _, r := range searchRows(t, srv, "/search?category=hdd") {
		byID[r.ID] = r.ProductID
	}
	if byID[rows[0].ID] != "p-ST1" {
		t.Errorf("resolved offer's row product_id = %q, want p-ST1", byID[rows[0].ID])
	}
	if byID[rows[1].ID] != "" {
		t.Errorf("unresolved offer's row carries product_id %q", byID[rows[1].ID])
	}
}

// With the offer layer off there is nothing to stamp, and nothing breaks.
func TestSearchRowsWithoutOfferLayer(t *testing.T) {
	srv := newTestServer(t)
	for _, r := range searchRows(t, srv, "/search?category=hdd") {
		if r.ProductID != "" {
			t.Fatalf("row %s has product_id %q with no offer layer", r.ID, r.ProductID)
		}
	}
}

// One failing watch is reported on ITS entry and the rest are answered: the
// delivery cron reads every watch from this one response, so a 500 here used
// to silence every ping.
func TestServeWatchesIsolatesAFailingWatch(t *testing.T) {
	srv := newTestServer(t)
	srv.watches = watch.Config{Watches: []watch.Watch{
		{Name: "ghost", Category: "wine"}, // no wine surface in this server
		{Name: "big-hdd", Category: "hdd", StrongVerdicts: []string{"great"}},
	}}
	rec := do(t, srv, http.MethodGet, "/watches")
	if rec.Code != http.StatusOK {
		t.Fatalf("/watches status = %d; one bad watch must not fail the response", rec.Code)
	}
	var body struct {
		Watches []struct {
			Name        string      `json:"name"`
			Error       string      `json:"error"`
			StrongCount int         `json:"strong_count"`
			Strong      []searchRow `json:"strong"`
		} `json:"watches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Watches) != 2 {
		t.Fatalf("want 2 watches, got %d", len(body.Watches))
	}
	if body.Watches[0].Error == "" || body.Watches[0].Strong == nil {
		t.Fatalf("failing watch = %+v, want an error and an empty (not null) strong list", body.Watches[0])
	}
	if body.Watches[1].Error != "" || body.Watches[1].StrongCount != 1 {
		t.Fatalf("healthy watch = %+v, want answered with 1 strong match", body.Watches[1])
	}
}
