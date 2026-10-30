package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
)

// swapConnector serves whatever raws the test last set, so one Ingester can
// see several passes over a changing catalogue. A non-nil *err fails the
// fetch; *incomplete reports a truncated walk.
type swapConnector struct {
	raws       *[]listing.Raw
	err        *error
	incomplete *bool
}

func (c swapConnector) SourceID() string { return "fake" }
func (c swapConnector) Fetch(context.Context) ([]listing.Raw, error) {
	if c.err != nil && *c.err != nil {
		return nil, *c.err
	}
	return *c.raws, nil
}

func (c swapConnector) FetchComplete() bool { return c.incomplete == nil || !*c.incomplete }

// titleRejectingExtractor rejects every listing whose title starts with "x",
// with a reason that names the title (so a changed listing changes the reason).
type titleRejectingExtractor struct{}

func (titleRejectingExtractor) Category() string { return "hdd" }

func (titleRejectingExtractor) Extract(_ context.Context, s listing.Sanitized) (item.Item, error) {
	if strings.HasPrefix(s.Title, "x") {
		return item.Item{}, fmt.Errorf("not wanted: %s", s.Title)
	}
	return fakeExtractor{}.Extract(context.Background(), s)
}

func countContaining(logs []string, sub string) int {
	n := 0
	for _, l := range logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func dropLogFixture(raws *[]listing.Raw) (*Ingester, *[]string) {
	var logs []string
	ing := &Ingester{
		Connector: swapConnector{raws: raws},
		Sanitizer: sanitize.Passthrough{}, Extractor: titleRejectingExtractor{}, Store: store.NewMemoryStore(),
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	return ing, &logs
}

func ingestN(t *testing.T, ing *Ingester, n int) {
	t.Helper()
	for range n {
		if _, err := ing.Ingest(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// The same listing dropped for the same reason on every pass is logged ONCE
// (nagus-c6l: 21,752 identical "extract dropped" lines in one week). The pass
// still counts it as a skip every time -- only the log line is deduplicated.
func TestUnchangedDropIsLoggedOnce(t *testing.T) {
	raws := []listing.Raw{raw("a", "x-ssd", 100, "1"), raw("b", "Seagate 16TB", 12000, "16")}
	ing, logs := dropLogFixture(&raws)
	for pass := range 3 {
		res, err := ing.Ingest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Skips) != 1 {
			t.Fatalf("pass %d: skips = %+v, want the one rejection every pass", pass, res.Skips)
		}
	}
	if n := countContaining(*logs, "extract dropped a"); n != 1 {
		t.Fatalf("logged the unchanged drop %d times, want 1: %q", n, *logs)
	}
}

// A listing whose drop reason changes is logged again: the log still records
// every distinct verdict.
func TestChangedDropReasonIsLoggedAgain(t *testing.T) {
	raws := []listing.Raw{raw("a", "x-one", 100, "1")}
	ing, logs := dropLogFixture(&raws)
	ingestN(t, ing, 2)
	raws = []listing.Raw{raw("a", "x-two", 100, "1")}
	ingestN(t, ing, 2)
	if n := countContaining(*logs, "extract dropped a"); n != 2 {
		t.Fatalf("logged %d drops, want 2 (one per distinct reason): %q", n, *logs)
	}
}

// A listing that disappears from the source and comes back is logged again,
// and the memory of drops holds only listings the last pass saw.
func TestDropMemoryForgetsListingsTheSourceNoLongerShows(t *testing.T) {
	raws := []listing.Raw{raw("a", "x-ssd", 100, "1")}
	ing, logs := dropLogFixture(&raws)
	ingestN(t, ing, 1)
	raws = []listing.Raw{raw("b", "Seagate 16TB", 12000, "16")}
	ingestN(t, ing, 1)
	if len(ing.dropped) != 0 {
		t.Fatalf("drop memory kept %d listing(s) the source no longer shows", len(ing.dropped))
	}
	raws = []listing.Raw{raw("a", "x-ssd", 100, "1")}
	ingestN(t, ing, 1)
	if n := countContaining(*logs, "extract dropped a"); n != 2 {
		t.Fatalf("logged %d drops, want 2 (the listing came back): %q", n, *logs)
	}
}

// A listing that stops being dropped and is later dropped again is logged
// again.
func TestDropAfterRecoveryIsLoggedAgain(t *testing.T) {
	raws := []listing.Raw{raw("a", "x-ssd", 100, "1")}
	ing, logs := dropLogFixture(&raws)
	ingestN(t, ing, 1)
	raws = []listing.Raw{raw("a", "Seagate 16TB", 12000, "16")}
	ingestN(t, ing, 1)
	raws = []listing.Raw{raw("a", "x-ssd", 100, "1")}
	ingestN(t, ing, 1)
	if n := countContaining(*logs, "extract dropped a"); n != 2 {
		t.Fatalf("logged %d drops, want 2: %q", n, *logs)
	}
}

type refusingSanitizer struct{ err error }

func (r refusingSanitizer) Sanitize(context.Context, listing.Raw) (listing.Sanitized, error) {
	return listing.Sanitized{}, r.err
}

// A glovebox quarantine is a verdict on the listing: deduplicated like an
// extract drop.
func TestUnchangedQuarantineIsLoggedOnce(t *testing.T) {
	raws := []listing.Raw{raw("a", "anything", 100, "1")}
	ing, logs := dropLogFixture(&raws)
	ing.Sanitizer = refusingSanitizer{err: fmt.Errorf("%w: score 0.91, signals x", sanitize.ErrQuarantined)}
	ingestN(t, ing, 3)
	if n := countContaining(*logs, "sanitize dropped a"); n != 1 {
		t.Fatalf("logged the unchanged quarantine %d times, want 1: %q", n, *logs)
	}
}

// A fail-closed gate failure is an OUTAGE, not a verdict on the listing: it
// is logged on every pass for as long as it lasts.
func TestGateOutageIsLoggedEveryPass(t *testing.T) {
	raws := []listing.Raw{raw("a", "anything", 100, "1")}
	ing, logs := dropLogFixture(&raws)
	ing.Sanitizer = refusingSanitizer{err: errors.New("sanitize: glovebox unreachable, dropping (fail closed): dial tcp: refused")}
	ingestN(t, ing, 3)
	if n := countContaining(*logs, "sanitize dropped a"); n != 3 {
		t.Fatalf("logged the gate outage %d times over 3 passes, want 3: %q", n, *logs)
	}
}

// A failed fetch changes nothing: the next good pass does not re-log drops
// the pass before it already logged.
func TestDropMemorySurvivesAFailedFetch(t *testing.T) {
	raws := []listing.Raw{raw("a", "x-ssd", 100, "1")}
	var fetchErr error
	ing, logs := dropLogFixture(&raws)
	ing.Connector = swapConnector{raws: &raws, err: &fetchErr}
	ingestN(t, ing, 1)
	fetchErr = errors.New("network down")
	if _, err := ing.Ingest(context.Background()); err == nil {
		t.Fatal("want the fetch error")
	}
	fetchErr = nil
	ingestN(t, ing, 1)
	if n := countContaining(*logs, "extract dropped a"); n != 1 {
		t.Fatalf("logged %d drops, want 1 (the failed fetch must not reset the memory): %q", n, *logs)
	}
}

// A truncated walk keeps the memory of listings it did not reach.
func TestIncompleteFetchKeepsUnreachedListingsInMemory(t *testing.T) {
	raws := []listing.Raw{raw("a", "x-ssd", 100, "1"), raw("b", "x-ssd", 100, "1")}
	incomplete := false
	ing, logs := dropLogFixture(&raws)
	ing.Connector = swapConnector{raws: &raws, incomplete: &incomplete}
	ingestN(t, ing, 1)
	full := raws
	raws = full[:1] // the walk stopped before "b"
	incomplete = true
	ingestN(t, ing, 1)
	raws = full
	incomplete = false
	ingestN(t, ing, 1)
	if n := countContaining(*logs, "extract dropped b"); n != 1 {
		t.Fatalf("logged b %d times, want 1 (an unreached listing is not new): %q", n, *logs)
	}
}

// The surface logs ONE summary line per request for its filtered items, not
// one line per item per request (nagus-c6l: ~28 items x 334 requests).
func TestSurfaceSummarisesFilteredItems(t *testing.T) {
	raws := []listing.Raw{
		raw("big", "Seagate 16TB", 12000, "16"),
		raw("s1", "tiny 4TB", 4000, "4"),
		raw("s2", "tiny 4TB again", 4000, "4"),
		raw("s3", "tiny 2TB", 2000, "2"),
	}
	st := store.NewMemoryStore()
	ing := &Ingester{Connector: fakeConnector{raws: raws}, Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	var logs []string
	s := &Surface{
		Store:  st,
		Filter: score.Filter{Category: "hdd", MinAttr: map[string]float64{"capacity_tb": 8}},
		Logf:   func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	res, err := s.Surface(context.Background(), store.Query{Category: "hdd"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Filtered != 1 {
		t.Fatalf("Filtered = %d, want 1", res.Filtered)
	}
	if n := countContaining(logs, "surface: filtered "); n != 0 {
		t.Fatalf("per-item filter lines remain: %q", logs)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "hdd: filtered out 3 of 4") {
		t.Fatalf("want one summary line, got %q", logs)
	}
	// The most common reason leads, with its count.
	if !strings.Contains(logs[0], `2x attribute "capacity_tb" value 4 is below minimum 8 (e.g. s`) {
		t.Fatalf("summary lacks the reason counts: %q", logs[0])
	}
}

// Nothing filtered, nothing logged.
func TestSurfaceLogsNothingWhenNothingIsFiltered(t *testing.T) {
	st := store.NewMemoryStore()
	ing := &Ingester{Connector: fakeConnector{raws: []listing.Raw{raw("big", "Seagate 16TB", 12000, "16")}}, Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	var logs []string
	s := &Surface{Store: st, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	if _, err := s.Surface(context.Background(), store.Query{Category: "hdd"}); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Fatalf("logged %q with nothing filtered", logs)
	}
}
