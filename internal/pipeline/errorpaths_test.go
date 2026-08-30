package pipeline

// Error-path tests for the pipeline package: these pin the CONTROLLED
// DEGRADATION contracts (CLAUDE.md "verification habits") -- a store, offer,
// sanitize, or valuate failure must be recorded and must NOT silently look
// like success (an item wrongly dropped, or wrongly kept, either one is the
// failure-that-looks-like-success this repo has been burned by before).
//
// All new identifiers here are prefixed errpath to avoid colliding with the
// existing fakeConnector/fakeExtractor/idConnector/partialConnector helpers
// in ingester_test.go and surface_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
)

// --- fakes ----------------------------------------------------------------

// errpathFailingOfferStore wraps a real offer.MemoryStore but lets each
// method be forced to fail independently, and records whether MarkExpired /
// ApplyRetention were actually invoked -- needed to prove the partial-fetch
// safety rule skips them ENTIRELY rather than calling-then-ignoring.
type errpathFailingOfferStore struct {
	inner *offer.MemoryStore

	putErr             error
	markExpiredErr     error
	applyRetentionErr  error
	markExpiredCalled  bool
	applyRetentionCall bool
}

func newErrpathFailingOfferStore() *errpathFailingOfferStore {
	return &errpathFailingOfferStore{inner: offer.NewMemoryStore()}
}

func (s *errpathFailingOfferStore) Put(ctx context.Context, o offer.Offer) error {
	if s.putErr != nil {
		return s.putErr
	}
	return s.inner.Put(ctx, o)
}

func (s *errpathFailingOfferStore) Get(ctx context.Context, id string) (offer.Offer, bool, error) {
	return s.inner.Get(ctx, id)
}

func (s *errpathFailingOfferStore) Query(ctx context.Context, q offer.Query) ([]offer.Offer, error) {
	return s.inner.Query(ctx, q)
}

func (s *errpathFailingOfferStore) MarkExpired(ctx context.Context, sourceID string, notSeenSince, now time.Time) (int, error) {
	s.markExpiredCalled = true
	if s.markExpiredErr != nil {
		return 0, s.markExpiredErr
	}
	return s.inner.MarkExpired(ctx, sourceID, notSeenSince, now)
}

func (s *errpathFailingOfferStore) ApplyRetention(ctx context.Context, sourceID string, r offer.Retention, now time.Time) (int, error) {
	s.applyRetentionCall = true
	if s.applyRetentionErr != nil {
		return 0, s.applyRetentionErr
	}
	return s.inner.ApplyRetention(ctx, sourceID, r, now)
}

// errpathFailingSanitizer always refuses, exercising the "sanitize dropped"
// skip path. A real Sanitizer must never fail open; this fake models the
// refusal side of that contract.
type errpathFailingSanitizer struct {
	err error
}

func (s errpathFailingSanitizer) Sanitize(context.Context, listing.Raw) (listing.Sanitized, error) {
	return listing.Sanitized{}, s.err
}

// errpathFailingItemStore wraps a real item store.MemoryStore but can force
// Put to fail, to exercise the "store dropped" skip path independent of the
// offer and sanitize paths.
type errpathFailingItemStore struct {
	inner  *store.MemoryStore
	putErr error
}

func newErrpathFailingItemStore() *errpathFailingItemStore {
	return &errpathFailingItemStore{inner: store.NewMemoryStore()}
}

func (s *errpathFailingItemStore) Put(ctx context.Context, it item.Item) error {
	if s.putErr != nil {
		return s.putErr
	}
	return s.inner.Put(ctx, it)
}

func (s *errpathFailingItemStore) Get(ctx context.Context, id string) (item.Item, bool, error) {
	return s.inner.Get(ctx, id)
}

func (s *errpathFailingItemStore) Search(ctx context.Context, q store.Query) ([]item.Item, error) {
	return s.inner.Search(ctx, q)
}

func (s *errpathFailingItemStore) DeleteStale(ctx context.Context, sourceID string, olderThan time.Time) (int, error) {
	return s.inner.DeleteStale(ctx, sourceID, olderThan)
}

// errpathSearchErrorStore is a minimal store.Store whose Search always fails,
// to prove Surface propagates a Store.Search error rather than swallowing it.
type errpathSearchErrorStore struct {
	err error
}

func (errpathSearchErrorStore) Put(context.Context, item.Item) error { return nil }
func (errpathSearchErrorStore) Get(context.Context, string) (item.Item, bool, error) {
	return item.Item{}, false, nil
}
func (s errpathSearchErrorStore) Search(context.Context, store.Query) ([]item.Item, error) {
	return nil, s.err
}
func (errpathSearchErrorStore) DeleteStale(context.Context, string, time.Time) (int, error) {
	return 0, nil
}

// --- ingester error paths ---------------------------------------------------

// An offer-store Put failure must be recorded as a Skip on stage "offer" and
// must NOT increment OffersRecorded -- but it must also NOT block the item
// path, because the item path feeds the live surface (see Ingester.Offers
// doc comment).
func TestErrpathOfferPutFailureDoesNotBlockItemPath(t *testing.T) {
	itemStore := store.NewMemoryStore()
	offers := newErrpathFailingOfferStore()
	offers.putErr = errors.New("offer backend unavailable")

	ing := &Ingester{
		Connector: idConnector{id: "shopify:x", raws: []listing.Raw{
			{SourceID: "shopify:x", SourceKey: "a", Title: "Drive A", PriceCents: 1000, Currency: "USD"},
		}},
		Sanitizer: sanitize.Passthrough{Name: "test"},
		Extractor: fakeExtractor{},
		Store:     itemStore,
		Offers:    offers,
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.OffersRecorded != 0 {
		t.Errorf("OffersRecorded = %d, want 0 (Put failed)", res.OffersRecorded)
	}
	if len(res.Skips) != 1 || res.Skips[0].Stage != "offer" || res.Skips[0].SourceKey != "a" {
		t.Fatalf("expected one offer-stage skip for 'a', got %+v", res.Skips)
	}
	if res.Stored != 1 {
		t.Errorf("Stored = %d, want 1 -- an offer failure must not block the item path", res.Stored)
	}
	if _, ok, _ := itemStore.Get(context.Background(), "a"); !ok {
		t.Fatal("item 'a' must still be stored despite the offer-store failure")
	}
}

// A Sanitizer error must be recorded as a Skip on stage "sanitize" and the
// listing must never reach the item store.
func TestErrpathSanitizeFailureSkipsAndDoesNotStore(t *testing.T) {
	st := store.NewMemoryStore()
	var logged []string
	ing := &Ingester{
		Connector: fakeConnector{raws: []listing.Raw{raw("a", "Seagate 16TB", 12000, "16")}},
		Sanitizer: errpathFailingSanitizer{err: errors.New("quarantined")},
		Extractor: fakeExtractor{},
		Store:     st,
		Logf: func(format string, args ...any) {
			logged = append(logged, format)
		},
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(res.Skips) != 1 || res.Skips[0].Stage != "sanitize" || res.Skips[0].SourceKey != "a" {
		t.Fatalf("expected one sanitize-stage skip for 'a', got %+v", res.Skips)
	}
	if res.Stored != 0 {
		t.Errorf("Stored = %d, want 0", res.Stored)
	}
	if _, ok, _ := st.Get(context.Background(), "a"); ok {
		t.Fatal("a sanitize failure must never reach the item store")
	}
	if len(logged) == 0 {
		t.Fatal("expected the sanitize failure to be logged via Logf")
	}
}

// An item-store Put failure must be recorded as a Skip on stage "store" and
// must not increment Stored.
func TestErrpathItemStorePutFailureSkips(t *testing.T) {
	st := newErrpathFailingItemStore()
	st.putErr = errors.New("disk full")
	ing := &Ingester{
		Connector: fakeConnector{raws: []listing.Raw{raw("a", "Seagate 16TB", 12000, "16")}},
		Sanitizer: sanitize.Passthrough{},
		Extractor: fakeExtractor{},
		Store:     st,
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(res.Skips) != 1 || res.Skips[0].Stage != "store" || res.Skips[0].SourceKey != "a" {
		t.Fatalf("expected one store-stage skip for 'a', got %+v", res.Skips)
	}
	if res.Stored != 0 {
		t.Errorf("Stored = %d, want 0", res.Stored)
	}
}

// A MarkExpired failure must leave OffersExpired at 0 and must not panic.
func TestErrpathMarkExpiredFailureLeavesCounterZero(t *testing.T) {
	offers := newErrpathFailingOfferStore()
	offers.markExpiredErr = errors.New("expire backend down")
	now := time.Unix(1_750_000_000, 0).UTC()
	if err := offers.inner.Put(context.Background(), offer.Offer{
		SourceID: "s", SourceKey: "old", PriceCents: 100, Currency: "USD",
		LastSeen: now.Add(-72 * time.Hour), Status: offer.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	ing := &Ingester{
		Connector:        idConnector{id: "s"},
		Offers:           offers,
		OfferExpireAfter: 24 * time.Hour,
		Now:              func() time.Time { return now },
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.OffersExpired != 0 {
		t.Fatalf("OffersExpired = %d, want 0 (MarkExpired failed)", res.OffersExpired)
	}
}

// An ApplyRetention failure must leave OffersPurged at 0 and must not panic.
// This includes the deliberate refusal of an unimplemented policy (see the
// keepOffers doc comment), so any ApplyRetention error must degrade the same
// way.
func TestErrpathApplyRetentionFailureLeavesCounterZero(t *testing.T) {
	offers := newErrpathFailingOfferStore()
	offers.applyRetentionErr = errors.New("retention backend down")
	now := time.Unix(1_750_000_000, 0).UTC()
	if err := offers.inner.Put(context.Background(), offer.Offer{
		SourceID: "s", SourceKey: "ancient", PriceCents: 100, Currency: "USD",
		LastSeen: now.Add(-48 * time.Hour), Status: offer.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	ing := &Ingester{
		Connector:      idConnector{id: "s"},
		Offers:         offers,
		OfferRetention: offer.Retention{Policy: offer.Purge, Window: 6 * time.Hour},
		Now:            func() time.Time { return now },
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.OffersPurged != 0 {
		t.Fatalf("OffersPurged = %d, want 0 (ApplyRetention failed)", res.OffersPurged)
	}
}

// A connector reporting an INCOMPLETE fetch must skip BOTH expiry and
// retention entirely -- not call-then-discard the result, but never call
// MarkExpired/ApplyRetention at all. This is the partial-fetch safety rule
// (nagus verification habit #2/#3: pagination capped silently, expiry after a
// partial fetch).
func TestErrpathIncompleteFetchSkipsExpiryAndRetentionEntirely(t *testing.T) {
	offers := newErrpathFailingOfferStore()
	now := time.Unix(1_750_000_000, 0).UTC()
	if err := offers.inner.Put(context.Background(), offer.Offer{
		SourceID: "shopify:big", SourceKey: "unseen", PriceCents: 100, Currency: "USD",
		LastSeen: now.Add(-72 * time.Hour), Status: offer.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	ing := &Ingester{
		Connector:        partialConnector{id: "shopify:big", complete: false},
		Offers:           offers,
		OfferExpireAfter: 24 * time.Hour,
		OfferRetention:   offer.Retention{Policy: offer.Purge, Window: 1 * time.Hour},
		Now:              func() time.Time { return now },
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.OffersExpired != 0 || res.OffersPurged != 0 {
		t.Fatalf("OffersExpired=%d OffersPurged=%d, want 0/0 after an incomplete fetch", res.OffersExpired, res.OffersPurged)
	}
	if offers.markExpiredCalled {
		t.Error("MarkExpired must not be called at all after an incomplete fetch")
	}
	if offers.applyRetentionCall {
		t.Error("ApplyRetention must not be called at all after an incomplete fetch")
	}
}

// logf with a nil Logf must not panic (it is optional).
func TestErrpathLogfNilDoesNotPanic(t *testing.T) {
	ing := &Ingester{}
	ing.logf("nothing listens: %d", 1)
}

// SourceID returns the underlying connector's SourceID when a connector is
// set.
func TestErrpathSourceIDReturnsConnectorID(t *testing.T) {
	ing := &Ingester{Connector: idConnector{id: "shopify:widgets"}}
	if got := ing.SourceID(); got != "shopify:widgets" {
		t.Fatalf("SourceID() = %q, want %q", got, "shopify:widgets")
	}
}

// SourceID with a nil Connector panics: it dereferences the (nil) interface
// directly with no nil guard. This pins the known behaviour rather than
// silently tolerating a change to it (or the reverse: someone silently
// swallowing the panic and returning "").
func TestErrpathSourceIDWithNilConnectorPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected SourceID() to panic with a nil Connector")
		}
	}()
	ing := &Ingester{}
	_ = ing.SourceID()
}

// --- surface error paths ----------------------------------------------------

// A Store.Search failure must propagate to the caller rather than be
// swallowed into an empty, successful-looking result.
func TestErrpathSurfaceSearchErrorPropagates(t *testing.T) {
	wantErr := errors.New("search backend unavailable")
	s := &Surface{Store: errpathSearchErrorStore{err: wantErr}}
	_, err := s.Surface(context.Background(), store.Query{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Surface error = %v, want %v", err, wantErr)
	}
}

// A Valuate failure must degrade to the unscored verdict WITHOUT dropping the
// item from the surfaced results (an enrichment outage must not hide
// candidates), and must log through Logf when set.
func TestErrpathValuateFailureDegradesButStillSurfaces(t *testing.T) {
	st := store.NewMemoryStore()
	ing := &Ingester{
		Connector: fakeConnector{raws: []listing.Raw{raw("a", "Seagate 16TB", 12000, "16")}},
		Sanitizer: sanitize.Passthrough{},
		Extractor: fakeExtractor{},
		Store:     st,
	}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var logged []string
	valuateErr := errors.New("valuation service down")
	s := &Surface{
		Store: st,
		Valuate: func(context.Context, item.Item) (score.DealSignal, error) {
			return score.DealSignal{}, valuateErr
		},
		Logf: func(format string, args ...any) {
			logged = append(logged, format)
		},
	}
	res, err := s.Surface(context.Background(), store.Query{})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("Items = %d, want 1 -- a valuation outage must not hide candidates", len(res.Items))
	}
	if res.Items[0].Signal.Verdict != "unknown-no-reference" {
		t.Fatalf("Signal.Verdict = %q, want %q", res.Items[0].Signal.Verdict, "unknown-no-reference")
	}
	if len(logged) == 0 {
		t.Fatal("expected the Valuate failure to be logged via Logf")
	}
}

// A filter rejection with Logf set must exercise the logging branch (and
// must not surface the rejected item).
func TestErrpathFilterRejectionLogs(t *testing.T) {
	st := store.NewMemoryStore()
	ing := &Ingester{
		Connector: fakeConnector{raws: []listing.Raw{raw("small", "tiny 4TB", 4000, "4")}},
		Sanitizer: sanitize.Passthrough{},
		Extractor: fakeExtractor{},
		Store:     st,
	}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var logged []string
	s := &Surface{
		Store:  st,
		Filter: score.Filter{Category: "hdd", MinAttr: map[string]float64{"capacity_tb": 8}},
		Logf: func(format string, args ...any) {
			logged = append(logged, format)
		},
	}
	res, err := s.Surface(context.Background(), store.Query{})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("Items = %d, want 0 -- 'small' fails the capacity filter", len(res.Items))
	}
	if len(logged) == 0 {
		t.Fatal("expected the filter rejection to be logged via Logf")
	}
}
