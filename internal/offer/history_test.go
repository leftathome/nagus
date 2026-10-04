package offer

import (
	"testing"
	"time"
)

func TestHistoryRowsRule(t *testing.T) {
	t0 := time.Unix(1_750_000_000, 0).UTC()
	t1, t2 := t0.Add(time.Hour), t0.Add(2*time.Hour)
	known := PriorPrice{Exists: true, PriceCents: 1000, Currency: "USD", ChangedAt: t0, HasHistory: true}
	cases := []struct {
		name          string
		prior         PriorPrice
		price         int64
		wantAnchor    *PricePoint
		wantRow       *PricePoint
		wantNeedProbe bool
	}{
		{"first insert, known", PriorPrice{}, 1000, nil, &PricePoint{t0, 1000, "USD"}, false},
		{"first insert, unknown", PriorPrice{}, 0, nil, nil, false},
		{"same price", known, 1000, nil, nil, false},
		{"unknown poll", known, 0, nil, nil, false},
		{"first known after unknown", PriorPrice{Exists: true, ChangedAt: t0}, 900, nil, &PricePoint{t1, 900, "USD"}, false},
		{"change with history", known, 800, nil, &PricePoint{t2, 800, "USD"}, true},
		{
			"change without history anchors",
			PriorPrice{Exists: true, PriceCents: 1000, Currency: "EUR", ChangedAt: t0},
			800,
			&PricePoint{t0, 1000, "EUR"}, &PricePoint{t2, 800, "USD"}, true,
		},
	}
	for _, c := range cases {
		changedAt := t2
		if !c.prior.Exists {
			changedAt = t0
		}
		a, r := HistoryRows(c.prior, c.price, "USD", t1, changedAt)
		if (a == nil) != (c.wantAnchor == nil) || (a != nil && *a != *c.wantAnchor) {
			t.Errorf("%s: anchor = %+v, want %+v", c.name, a, c.wantAnchor)
		}
		if (r == nil) != (c.wantRow == nil) || (r != nil && *r != *c.wantRow) {
			t.Errorf("%s: row = %+v, want %+v", c.name, r, c.wantRow)
		}
		if got := HistoryNeedsProbe(c.prior, c.price); got != c.wantNeedProbe {
			t.Errorf("%s: HistoryNeedsProbe = %v, want %v", c.name, got, c.wantNeedProbe)
		}
	}
}

func TestHistoryCutoff(t *testing.T) {
	now := time.Unix(1_750_000_000, 0).UTC()
	if c, ok := (Retention{Policy: Purge, Window: 6 * time.Hour, HistoryWindow: 999 * time.Hour}).HistoryCutoff(now); !ok || !c.Equal(now.Add(-6*time.Hour)) {
		t.Fatalf("purge: cutoff %v %v, want the offer window", c, ok)
	}
	if c, ok := (Retention{Policy: RetainFull, HistoryWindow: 24 * time.Hour}).HistoryCutoff(now); !ok || !c.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("retain-full with window: %v %v", c, ok)
	}
	if _, ok := (Retention{Policy: RetainFull}).HistoryCutoff(now); ok {
		t.Fatal("retain-full without a window must keep history")
	}
}

func TestPriceAt(t *testing.T) {
	t0 := time.Unix(1_750_000_000, 0).UTC()
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	sm := Summary{Status: StatusActive, FirstSeen: h(0), PriceCents: 800, PriceChangedAt: h(5), PreviousPriceCents: 1000}
	hist := []PricePoint{{h(0), 1000, "USD"}, {h(5), 800, "USD"}}
	for _, c := range []struct {
		at     time.Time
		want   int64
		wantOK bool
	}{
		{h(-1), 0, false}, {h(0), 1000, true}, {h(4), 1000, true}, {h(5), 800, true}, {h(9), 800, true},
	} {
		if got, ok := PriceAt(sm, hist, c.at); got != c.want || ok != c.wantOK {
			t.Errorf("PriceAt(%v) = %d %v, want %d %v", c.at, got, ok, c.want, c.wantOK)
		}
	}
	// Before the earliest retained point: unknown, not guessed.
	if _, ok := PriceAt(sm, hist[1:], h(3)); ok {
		t.Error("a time before the earliest point must be unknown")
	}
	// No history: only the current price from its change on; before it,
	// unknown (previous_price_cents is not used).
	if got, ok := PriceAt(sm, nil, h(3)); ok {
		t.Errorf("no history, before the change: %d %v, want unknown", got, ok)
	}
	if got, ok := PriceAt(sm, nil, h(6)); !ok || got != 800 {
		t.Errorf("no history, after the change: %d %v", got, ok)
	}
	// Expired: no price from the expiry on.
	exp := sm
	exp.Status, exp.ExpiredAt = StatusExpired, h(7)
	if _, ok := PriceAt(exp, hist, h(8)); ok {
		t.Error("an expired offer has no price after it expired")
	}
	if got, ok := PriceAt(exp, hist, h(6)); !ok || got != 800 {
		t.Errorf("expired offer before its expiry: %d %v", got, ok)
	}
}

// Two changes, then the history purged (eBay's 6h window): seen at 1000 four
// days ago, 900 at 2.5 days ago, 800 a day ago. The offer row says
// previous = 900, but 3 days ago the price was 1000: before price_changed_at
// the price must be UNKNOWN, never previous_price_cents.
func TestPriceAtTwoChangesNoHistory(t *testing.T) {
	now := time.Unix(1_750_000_000, 0).UTC()
	ago := func(days float64) time.Time { return now.Add(-time.Duration(days * float64(24*time.Hour))) }
	sm := Summary{Status: StatusActive, FirstSeen: ago(4), PriceCents: 800, PriceChangedAt: ago(1), PreviousPriceCents: 900}
	for _, at := range []time.Time{ago(3), ago(2), ago(1.1)} {
		if p, ok := PriceAt(sm, nil, at); ok {
			t.Errorf("PriceAt(%v) = %d, want unknown", at, p)
		}
	}
	if p, ok := PriceAt(sm, nil, ago(0.5)); !ok || p != 800 {
		t.Errorf("after the last change: %d %v, want 800", p, ok)
	}
	// With the 900 point still recorded, 2 days ago is known.
	hist := []PricePoint{{ago(2.5), 900, "USD"}, {ago(1), 800, "USD"}}
	if p, ok := PriceAt(sm, hist, ago(2)); !ok || p != 900 {
		t.Errorf("with history, 2 days ago: %d %v, want 900", p, ok)
	}
	if _, ok := PriceAt(sm, hist, ago(3)); ok {
		t.Error("before the earliest point must be unknown")
	}
}
