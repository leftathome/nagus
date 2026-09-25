package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/connector/ebay"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
)

// loopFakeConnector is a listing.Connector with a configurable, fixed
// Fetch response (raws and/or an error) and an atomic call counter, for
// exercising startIngest / runSourceIngestLoop / ingestOnceSource (serve.go).
// Always use *loopFakeConnector (never a value copy: atomic.Int64 must not be
// copied after first use). The configured fields are set once at construction
// and never mutated afterward, so concurrent Fetch calls reading them are race
// free without extra locking.
type loopFakeConnector struct {
	id   string
	raws []listing.Raw
	err  error

	calls atomic.Int64
}

func (c *loopFakeConnector) SourceID() string { return c.id }

func (c *loopFakeConnector) Fetch(context.Context) ([]listing.Raw, error) {
	c.calls.Add(1)
	return c.raws, c.err
}

// loopWaitForCount polls got (an atomic counter read) until it reaches at
// least want, or fails the test once deadline elapses. Used instead of a fixed
// sleep so the ticking tests stay fast on quiet CI and don't hang on slow CI.
func loopWaitForCount(t *testing.T, deadline time.Duration, want int64, got func() int64) {
	t.Helper()
	cutoff := time.Now().Add(deadline)
	for {
		if got() >= want {
			return
		}
		if time.Now().After(cutoff) {
			t.Fatalf("count = %d after %s, want >= %d", got(), deadline, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- ingestOnceSource -----------------------------------------------------

// Success path: the connector is called and a normal batch is ingested
// without panicking. No Sanitizer/Extractor/Store is configured (the
// offer-only shape from internal/pipeline), which is enough to exercise
// ingestOnceSource's success branch (it only depends on Ingester.Ingest
// returning a nil error).
func TestIngestOnceSourceSuccessNoOffers(t *testing.T) {
	conn := &loopFakeConnector{id: "loop-success", raws: []listing.Raw{
		{SourceID: "loop-success", SourceKey: "a", Title: "Drive A", PriceCents: 1000, Currency: "USD"},
		{SourceID: "loop-success", SourceKey: "b", Title: "Drive B", PriceCents: 2000, Currency: "USD"},
	}}
	ing := &pipeline.Ingester{Connector: conn}

	ingestOnceSource(context.Background(), ing) // must not panic

	if got := conn.calls.Load(); got != 1 {
		t.Fatalf("connector Fetch called %d times, want 1", got)
	}
}

// Success path with the offer layer wired in, so OffersRecorded > 0 and
// ingestOnceSource's "offers=..." log-line branch actually executes (still
// asserted only via call count / store state, never log text).
func TestIngestOnceSourceSuccessWithOffers(t *testing.T) {
	conn := &loopFakeConnector{id: "loop-offers", raws: []listing.Raw{
		{SourceID: "loop-offers", SourceKey: "a", Title: "Drive A", PriceCents: 1000, Currency: "USD"},
	}}
	offers := offer.NewMemoryStore()
	ing := &pipeline.Ingester{Connector: conn, Offers: offers}

	ingestOnceSource(context.Background(), ing) // must not panic

	if got := conn.calls.Load(); got != 1 {
		t.Fatalf("connector Fetch called %d times, want 1", got)
	}
	if offers.Len() != 1 {
		t.Fatalf("offer store holds %d, want 1 (needed so OffersRecorded > 0 and the offers log branch runs)", offers.Len())
	}
}

// Generic connector error: ingestOnceSource must swallow it (no panic, no
// propagation -- there is nothing to propagate to, it returns nothing).
func TestIngestOnceSourceGenericErrorSwallowed(t *testing.T) {
	conn := &loopFakeConnector{id: "loop-err", err: errors.New("loop fake: network down")}
	ing := &pipeline.Ingester{Connector: conn}

	ingestOnceSource(context.Background(), ing) // must not panic

	if got := conn.calls.Load(); got != 1 {
		t.Fatalf("connector Fetch called %d times, want 1", got)
	}
}

// Budget-exhausted connector error (wrapped, as the eBay connector wraps it):
// ingestOnceSource must take the back-off branch and swallow it, same as any
// other error, without panicking.
func TestIngestOnceSourceBudgetExhaustedSwallowed(t *testing.T) {
	conn := &loopFakeConnector{id: "loop-budget", err: fmt.Errorf("ebay fetch: %w", ebay.ErrBudgetExhausted)}
	ing := &pipeline.Ingester{Connector: conn}

	ingestOnceSource(context.Background(), ing) // must not panic; takes the budget back-off branch

	if got := conn.calls.Load(); got != 1 {
		t.Fatalf("connector Fetch called %d times, want 1", got)
	}
}

// --- runSourceIngestLoop ----------------------------------------------------

// runSourceIngestLoop must run an immediate ingest, then keep ingesting on
// every tick, and must actually return once ctx is cancelled.
func TestRunSourceIngestLoopTicksAndReturnsOnCancel(t *testing.T) {
	conn := &loopFakeConnector{id: "loop-tick"}
	ing := &pipeline.Ingester{Connector: conn}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSourceIngestLoop(ctx, ing, 10*time.Millisecond)
		close(done)
	}()

	// Immediate run (1) plus at least one tick (2).
	loopWaitForCount(t, 2*time.Second, 2, conn.calls.Load)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runSourceIngestLoop did not return within 2s of context cancellation")
	}
}

// --- startIngest ------------------------------------------------------------

// startIngest must gate scheduling per-source on that source's own interval
// (a non-positive interval never schedules that source), and must isolate one
// source's persistent connector failure from another source's loop: the bad
// source keeps erroring on every tick while the good source keeps fetching
// right alongside it.
func TestStartIngestPerSourceIsolationAndIntervalGating(t *testing.T) {
	disabledConn := &loopFakeConnector{id: "loop-disabled"}
	badConn := &loopFakeConnector{id: "loop-bad", err: errors.New("loop fake: always fails")}
	goodConn := &loopFakeConnector{id: "loop-good"}

	srv := &server{ingesters: []*pipeline.Ingester{
		{Connector: disabledConn},
		{Connector: badConn},
		{Connector: goodConn},
	}}
	intervals := []time.Duration{0, 10 * time.Millisecond, 10 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.startIngest(ctx, intervals)

	// Both the erroring and the healthy source must keep advancing past their
	// immediate run and into ticks; the bad source's errors must never stall it.
	loopWaitForCount(t, 2*time.Second, 2, goodConn.calls.Load)
	loopWaitForCount(t, 2*time.Second, 2, badConn.calls.Load)

	if got := disabledConn.calls.Load(); got != 0 {
		t.Fatalf("disabled source (interval <= 0) was fetched %d times, want 0", got)
	}
}
