package enrich

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/quark"
)

var t0 = time.Unix(1_788_000_000, 0).UTC()

// fakeQuark resolves any hint with an MPN, refuses the rest, and records every
// call so tests can assert batching and the retry flag.
type fakeQuark struct {
	mu         sync.Mutex
	calls      []call
	generation int64
	err        error
	// beforeReply runs after the request is received and before the answer is
	// returned -- the window in which a real ingest can change a hint.
	beforeReply func()
}

type call struct {
	hints []quark.Hint
	retry bool
}

func (f *fakeQuark) Resolve(_ context.Context, hints []quark.Hint, retry bool) (quark.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{hints: hints, retry: retry})
	f.mu.Unlock()
	if f.beforeReply != nil {
		f.beforeReply()
	}
	if f.err != nil {
		return quark.Response{}, f.err
	}
	out := quark.Response{CatalogGeneration: f.generation, Results: make([]quark.Result, len(hints))}
	for i, h := range hints {
		if h.MPN == "" {
			out.Results[i] = quark.Result{Route: quark.RouteRefused}
			continue
		}
		out.Results[i] = quark.Result{Route: quark.RouteMinted, ProductID: "p-" + h.MPN}
	}
	return out, nil
}

func seed(t *testing.T, s offer.Store, source, key, mpn string) offer.Offer {
	t.Helper()
	o := offer.Offer{SourceID: source, SourceKey: key, PriceCents: 100, LastSeen: t0,
		ProductHint: offer.ProductHint{Brand: "Seagate", MPN: mpn}}
	if err := s.Put(context.Background(), o); err != nil {
		t.Fatalf("Put: %v", err)
	}
	o.ID = offer.DeterministicID(source, key)
	return o
}

func state(t *testing.T, s offer.Store, o offer.Offer) offer.Resolution {
	t.Helper()
	got, ok, err := s.Get(context.Background(), o.ID)
	if err != nil || !ok {
		t.Fatalf("Get: %v %v", ok, err)
	}
	return got.Resolution
}

func newEnricher(s offer.Store, q Resolver) *Enricher {
	return &Enricher{
		Offers:      s,
		Quark:       q,
		CategoryFor: func(src string) string { return map[string]string{"shopify:spd": "hdd"}[src] },
		Now:         func() time.Time { return t0 },
	}
}

func TestPassStampsResolvedAndRefused(t *testing.T) {
	s := offer.NewMemoryStore()
	a := seed(t, s, "shopify:spd", "a", "ST1")
	b := seed(t, s, "shopify:spd", "b", "")
	q := &fakeQuark{}
	res, err := newEnricher(s, q).RunPass(context.Background())
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if res.Recorded != 2 {
		t.Fatalf("recorded %d, want 2", res.Recorded)
	}
	if r := state(t, s, a); r.State != offer.ResolutionResolved || r.ProductID != "p-ST1" {
		t.Errorf("a = %+v", r)
	}
	if r := state(t, s, b); r.State != offer.ResolutionRefused || r.ProductID != "" {
		t.Errorf("b = %+v", r)
	}
	if q.calls[0].hints[0].Category != "hdd" {
		t.Errorf("category from source config not applied: %+v", q.calls[0].hints[0])
	}
}

func TestSecondPassSendsNothing(t *testing.T) {
	s := offer.NewMemoryStore()
	seed(t, s, "shopify:spd", "a", "ST1")
	seed(t, s, "shopify:spd", "b", "")
	q := &fakeQuark{}
	e := newEnricher(s, q)
	_, _ = e.RunPass(context.Background())
	before := len(q.calls)
	if _, err := e.RunPass(context.Background()); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if len(q.calls) != before {
		t.Fatalf("second pass made %d calls; resolved and refused offers must not be re-sent at the same generation", len(q.calls)-before)
	}
}

func TestQuarkFailureLeavesOffersUnattempted(t *testing.T) {
	s := offer.NewMemoryStore()
	a := seed(t, s, "shopify:spd", "a", "ST1")
	q := &fakeQuark{err: errors.New("connection refused")}
	e := newEnricher(s, q)
	if _, err := e.RunPass(context.Background()); err == nil {
		t.Fatal("a failed quark call must surface as the pass error")
	}
	if r := state(t, s, a); r.State != offer.ResolutionUnattempted {
		t.Fatalf("a failed call changed state: %+v", r)
	}
	if e.Snapshot().BatchesFailed != 1 {
		t.Fatalf("failure not counted: %+v", e.Snapshot())
	}
}

func TestUnauthorizedIsCountedDistinctly(t *testing.T) {
	s := offer.NewMemoryStore()
	seed(t, s, "shopify:spd", "a", "ST1")
	e := newEnricher(s, &fakeQuark{err: quark.ErrUnauthorized})
	_, err := e.RunPass(context.Background())
	if !errors.Is(err, quark.ErrUnauthorized) || e.Snapshot().Unauthorized != 1 {
		t.Fatalf("err=%v snapshot=%+v", err, e.Snapshot())
	}
}

func TestHintChangedMidFlightIsDiscarded(t *testing.T) {
	s := offer.NewMemoryStore()
	a := seed(t, s, "shopify:spd", "a", "ST1")
	q := &fakeQuark{}
	q.beforeReply = func() {
		q.beforeReply = nil
		// Ingest updates the listing while quark is answering about ST1.
		seed(t, s, "shopify:spd", "a", "ST2")
	}
	e := newEnricher(s, q)
	res, err := e.RunPass(context.Background())
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if res.Discarded != 1 {
		t.Fatalf("discarded %d, want 1", res.Discarded)
	}
	// The stale answer (p-ST1) must not be on the offer; the pass then resolves
	// the NEW hint in its next batch.
	if r := state(t, s, a); r.ProductID != "p-ST2" {
		t.Fatalf("offer resolution = %+v, want the new hint's product p-ST2", r)
	}
}

func TestRetriesOnlyAfterGenerationAdvanceAndAreLabelled(t *testing.T) {
	s := offer.NewMemoryStore()
	b := seed(t, s, "shopify:spd", "b", "") // refused
	q := &fakeQuark{}
	e := newEnricher(s, q)
	if _, err := e.RunPass(context.Background()); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if len(q.calls) != 1 || q.calls[0].retry {
		t.Fatalf("first pass calls = %+v", q.calls)
	}

	// quark's catalogue grows: the next answer reports generation 1. Any call
	// reveals it; seed a fresh offer so there is one.
	q.generation = 1
	seed(t, s, "shopify:spd", "c", "ST3")
	if _, err := e.RunPass(context.Background()); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	var retried bool
	for _, c := range q.calls[1:] {
		if c.retry {
			retried = true
			if len(c.hints) != 1 {
				t.Errorf("retry batch = %d hints, want only the refused offer", len(c.hints))
			}
		}
	}
	if !retried {
		t.Fatal("a refusal recorded under generation 0 was not re-offered after quark reported generation 1")
	}
	if r := state(t, s, b); r.Generation != 1 {
		t.Fatalf("retried refusal generation = %d, want 1", r.Generation)
	}
	if e.Snapshot().Retries != 1 {
		t.Fatalf("retries counted = %d, want 1", e.Snapshot().Retries)
	}
}

func TestBatchesRespectTheCap(t *testing.T) {
	s := offer.NewMemoryStore()
	for i := 0; i < 1203; i++ {
		seed(t, s, "shopify:spd", fmt.Sprintf("k%04d", i), fmt.Sprintf("M%d", i))
	}
	q := &fakeQuark{}
	res, err := newEnricher(s, q).RunPass(context.Background())
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if res.Recorded != 1203 || len(q.calls) != 3 {
		t.Fatalf("recorded=%d calls=%d, want 1203 in 3 batches", res.Recorded, len(q.calls))
	}
	for _, c := range q.calls {
		if len(c.hints) > quark.MaxBatch {
			t.Fatalf("batch of %d exceeds quark's cap", len(c.hints))
		}
	}
}

func TestEmptyHintIsRecordedLocallyWithoutACall(t *testing.T) {
	s := offer.NewMemoryStore()
	blank := offer.Offer{SourceID: "shopify:waterpanther", SourceKey: "wp1", PriceCents: 100, LastSeen: t0}
	if err := s.Put(context.Background(), blank); err != nil {
		t.Fatal(err)
	}
	blank.ID = offer.DeterministicID(blank.SourceID, blank.SourceKey)
	hinted := seed(t, s, "shopify:spd", "a", "ST1")

	q := &fakeQuark{}
	e := newEnricher(s, q)
	if _, err := e.RunPass(context.Background()); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if r := state(t, s, blank); r.State != offer.ResolutionUnidentifiable {
		t.Fatalf("empty-hint offer = %+v, want unidentifiable", r)
	}
	for _, c := range q.calls {
		for _, h := range c.hints {
			if h.Brand == "" && h.MPN == "" && h.GTIN == "" && h.Model == "" {
				t.Fatal("an empty hint was sent to quark")
			}
		}
	}
	if r := state(t, s, hinted); r.State != offer.ResolutionResolved {
		t.Fatalf("hinted offer = %+v", r)
	}
	if e.Snapshot().Unidentifiable != 1 {
		t.Fatalf("unidentifiable count = %d", e.Snapshot().Unidentifiable)
	}
	// And a batch of ONLY blank offers makes no call at all.
	s2 := offer.NewMemoryStore()
	_ = s2.Put(context.Background(), offer.Offer{SourceID: "x", SourceKey: "1", LastSeen: t0})
	q2 := &fakeQuark{}
	if _, err := newEnricher(s2, q2).RunPass(context.Background()); err != nil || len(q2.calls) != 0 {
		t.Fatalf("calls=%d err=%v; an all-blank pass must not call quark", len(q2.calls), err)
	}
}

// LastCleanPass is the staleness signal the stall alert reads. A clean pass
// stamps it -- including one with nothing to do -- and a failed pass does not.
func TestLastCleanPassTracksOnlyCleanPasses(t *testing.T) {
	s := offer.NewMemoryStore()
	q := &fakeQuark{}
	e := newEnricher(s, q)
	if e.Snapshot().LastCleanPass != 0 {
		t.Fatal("LastCleanPass must start at 0 (never)")
	}
	e.pass(context.Background()) // nothing pending: still clean
	if got := e.Snapshot().LastCleanPass; got != t0.Unix() {
		t.Fatalf("after an idle clean pass LastCleanPass = %d, want %d", got, t0.Unix())
	}

	seed(t, s, "shopify:spd", "a", "ST1")
	q.err = errors.New("connection refused")
	e.Now = func() time.Time { return t0.Add(time.Hour) }
	e.pass(context.Background())
	if got := e.Snapshot().LastCleanPass; got != t0.Unix() {
		t.Fatalf("a failed pass moved LastCleanPass to %d", got)
	}
}
