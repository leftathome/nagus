package offerstoretest

import (
	"context"
	"sort"
	"testing"

	"github.com/leftathome/nagus/internal/offer"
)

// The quark resolution contract (quark design section 5, "Coupling (D4)").
//
// The load-bearing rule is that INGEST MUST NOT ERASE RESOLUTION. Put is an
// upsert run on every ingest pass, so an adapter that simply overwrites the row
// would reset every resolved offer to unattempted each pass and nagus would ask
// quark about its whole catalogue forever. Conversely, a hint that CHANGED must
// reset, or nagus would keep a product id for an identifier the source no
// longer states.

func hinted(source, key, mpn string) offer.Offer {
	o := Offer(source, key, 100, T0)
	o.ProductHint = offer.ProductHint{Brand: "Seagate", MPN: mpn}
	return o
}

func get(t *testing.T, s offer.Store, o offer.Offer) offer.Offer {
	t.Helper()
	got, ok, err := s.Get(context.Background(), offer.DeterministicID(o.SourceID, o.SourceKey))
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	return got
}

func record(t *testing.T, s offer.Store, o offer.Offer, r offer.Resolution) bool {
	t.Helper()
	applied, err := s.RecordResolution(context.Background(),
		offer.DeterministicID(o.SourceID, o.SourceKey), o.ProductHint.Fingerprint(), r)
	if err != nil {
		t.Fatalf("RecordResolution: %v", err)
	}
	return applied
}

func newOfferIsUnattempted(t *testing.T, s offer.Store) {
	o := hinted("shop:a", "k1", "ST1")
	put(t, s, o)
	if got := get(t, s, o).Resolution.State; got != offer.ResolutionUnattempted {
		t.Fatalf("new offer state = %q, want unattempted", got)
	}
}

func putCannotSetResolution(t *testing.T, s offer.Store) {
	o := hinted("shop:a", "k1", "ST1")
	o.Resolution = offer.Resolution{State: offer.ResolutionResolved, ProductID: "forged", Generation: 9}
	put(t, s, o)
	got := get(t, s, o).Resolution
	if got.State != offer.ResolutionUnattempted || got.ProductID != "" {
		t.Fatalf("Put wrote resolution %+v; only RecordResolution may", got)
	}
}

func recordResolutionStamps(t *testing.T, s offer.Store) {
	o := hinted("shop:a", "k1", "ST1")
	put(t, s, o)
	want := offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-1", Generation: 3, At: T1}
	if !record(t, s, o, want) {
		t.Fatal("RecordResolution did not apply to a current hint")
	}
	got := get(t, s, o).Resolution
	if got.State != want.State || got.ProductID != want.ProductID || got.Generation != want.Generation || !got.At.Equal(want.At) {
		t.Fatalf("resolution = %+v, want %+v", got, want)
	}
}

func reingestSameHintPreservesResolution(t *testing.T, s offer.Store) {
	o := hinted("shop:a", "k1", "ST1")
	put(t, s, o)
	record(t, s, o, offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-1", Generation: 1, At: T1})

	again := hinted("shop:a", "k1", "ST1")
	again.PriceCents = 90 // an ordinary re-ingest: price moved, hint did not
	again.LastSeen = T2
	put(t, s, again)

	got := get(t, s, o).Resolution
	if got.State != offer.ResolutionResolved || got.ProductID != "p-1" {
		t.Fatalf("re-ingest with the same hint erased resolution: %+v", got)
	}
}

func reingestChangedHintResets(t *testing.T, s offer.Store) {
	o := hinted("shop:a", "k1", "ST1")
	put(t, s, o)
	record(t, s, o, offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-1", Generation: 1, At: T1})

	changed := hinted("shop:a", "k1", "ST2") // the source now states a different MPN
	changed.LastSeen = T2
	put(t, s, changed)

	got := get(t, s, o).Resolution
	if got.State != offer.ResolutionUnattempted || got.ProductID != "" || got.Generation != 0 {
		t.Fatalf("a changed hint kept a stale resolution: %+v", got)
	}
}

func recordResolutionRefusesStaleHint(t *testing.T, s offer.Store) {
	o := hinted("shop:a", "k1", "ST1")
	put(t, s, o)
	stale := o.ProductHint.Fingerprint()

	changed := hinted("shop:a", "k1", "ST2")
	changed.LastSeen = T1
	put(t, s, changed) // ingest raced the resolve call

	applied, err := s.RecordResolution(context.Background(),
		offer.DeterministicID(o.SourceID, o.SourceKey), stale,
		offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-for-ST1", At: T1})
	if err != nil {
		t.Fatalf("RecordResolution: %v", err)
	}
	if applied {
		t.Fatal("an answer about the previous hint was written onto the new one")
	}
	if got := get(t, s, o).Resolution; got.State != offer.ResolutionUnattempted {
		t.Fatalf("state = %+v, want unattempted", got)
	}
}

func recordResolutionOnMissingOffer(t *testing.T, s offer.Store) {
	applied, err := s.RecordResolution(context.Background(), "no-such-offer", "|||",
		offer.Resolution{State: offer.ResolutionRefused, At: T1})
	if err != nil {
		t.Fatalf("RecordResolution on a missing offer must not error (retention may have deleted it): %v", err)
	}
	if applied {
		t.Fatal("a resolution applied to an offer that does not exist")
	}
}

func pendingResolutionSelection(t *testing.T, s offer.Store) {
	un := hinted("shop:a", "un", "ST1")
	res := hinted("shop:a", "res", "ST2")
	refOld := hinted("shop:a", "ref-old", "ST3")
	refNew := hinted("shop:a", "ref-new", "ST4")
	quar := hinted("shop:a", "quar", "ST5")
	expired := hinted("shop:a", "expired", "ST6")
	for _, o := range []offer.Offer{un, res, refOld, refNew, quar, expired} {
		put(t, s, o)
	}
	record(t, s, res, offer.Resolution{State: offer.ResolutionResolved, ProductID: "p", Generation: 1, At: T1})
	record(t, s, refOld, offer.Resolution{State: offer.ResolutionRefused, Generation: 1, At: T1})
	record(t, s, refNew, offer.Resolution{State: offer.ResolutionRefused, Generation: 2, At: T1})
	record(t, s, quar, offer.Resolution{State: offer.ResolutionQuarantined, Generation: 1, At: T1})
	if _, err := s.MarkExpired(context.Background(), "shop:a", T1, T1); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}

	keys := func(retryBelow int64) []string {
		got, err := s.PendingResolution(context.Background(), 0, retryBelow)
		if err != nil {
			t.Fatalf("PendingResolution: %v", err)
		}
		out := make([]string, 0, len(got))
		for _, o := range got {
			out = append(out, o.SourceKey)
		}
		sort.Strings(out)
		return out
	}

	// No retries: only never-asked offers, expired ones included.
	if got, want := keys(0), []string{"expired", "un"}; !equal(got, want) {
		t.Errorf("retries off: pending = %v, want %v", got, want)
	}
	// Generation 2 reached: refusals recorded under generation 1 are re-offered;
	// the one recorded under 2 is not; resolved and quarantined never are.
	if got, want := keys(2), []string{"expired", "ref-old", "un"}; !equal(got, want) {
		t.Errorf("retry below 2: pending = %v, want %v", got, want)
	}
}

func pendingResolutionLimitPrefersUnattempted(t *testing.T, s offer.Store) {
	for _, k := range []string{"a", "b", "c"} {
		put(t, s, hinted("shop:a", "un-"+k, "U"+k))
	}
	ref := hinted("shop:a", "ref", "R1")
	put(t, s, ref)
	record(t, s, ref, offer.Resolution{State: offer.ResolutionRefused, Generation: 0, At: T1})

	got, err := s.PendingResolution(context.Background(), 2, 5)
	if err != nil {
		t.Fatalf("PendingResolution: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit 2 returned %d offers", len(got))
	}
	for _, o := range got {
		if o.Resolution.State != offer.ResolutionUnattempted {
			t.Errorf("a retry (%s) displaced a never-asked offer under the limit", o.SourceKey)
		}
	}
}

func queryByProductID(t *testing.T, s offer.Store) {
	a := hinted("shop:a", "a", "ST1")
	b := hinted("shop:b", "b", "ST1")
	c := hinted("shop:a", "c", "ST9")
	for _, o := range []offer.Offer{a, b, c} {
		put(t, s, o)
	}
	record(t, s, a, offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-1", At: T1})
	record(t, s, b, offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-1", At: T1})
	record(t, s, c, offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-9", At: T1})

	got, err := s.Query(context.Background(), offer.Query{ProductID: "p-1"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ProductID filter returned %d offers, want the 2 sellers of p-1", len(got))
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
