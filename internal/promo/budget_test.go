package promo

import (
	"testing"
	"time"
)

func TestBudget(t *testing.T) {
	now := time.Date(2026, 10, 16, 23, 0, 0, 0, time.UTC)
	b := NewBudget(9, func() time.Time { return now })
	p := b.NewPoll(6)
	for i := 0; i < 3; i++ {
		if ok, _ := p.Reserve(2); !ok {
			t.Fatalf("reserve %d refused", i)
		}
	}
	if ok, scope := p.Reserve(2); ok || scope != "poll" {
		t.Fatalf("poll cap: %v %q", ok, scope)
	}
	// A new poll has its own allowance, against the same day.
	p2 := b.NewPoll(6)
	if ok, _ := p2.Reserve(2); !ok {
		t.Fatal("second poll refused")
	}
	if ok, scope := p2.Reserve(2); ok || scope != "day" {
		t.Fatalf("day cap: %v %q (used %d)", ok, scope, b.Stats().UsedToday)
	}
	// All or nothing: a refused reservation takes nothing.
	if st := b.Stats(); st.UsedToday != 8 || st.Exhausted["poll"] != 1 || st.Exhausted["day"] != 1 {
		t.Fatalf("stats %+v", st)
	}
	// UTC midnight resets the day.
	now = now.Add(2 * time.Hour)
	if ok, _ := p2.Reserve(2); !ok {
		t.Fatal("new day refused")
	}
	if b.Stats().UsedToday != 2 {
		t.Fatalf("used after rollover %d", b.Stats().UsedToday)
	}
	if d := NewBudget(0, nil); d.PerDay != DefaultCallsPerDay || d.NewPoll(0).PerPoll != DefaultCallsPerPoll {
		t.Fatal("defaults")
	}
}
