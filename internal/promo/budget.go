package promo

import (
	"context"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/offer"
)

// Default vision call budgets. The GPU host is shared.
const (
	DefaultCallsPerPoll = 60
	DefaultCallsPerDay  = 600
)

// Budget is the process-wide daily vision call budget (UTC day), shared by
// every promo source. Each source poll has its own per-poll allowance
// (PollBudget). Safe for concurrent use.
type Budget struct {
	PerDay int
	Now    func() time.Time
	// State, when set, persists each day's calls in the offers database
	// (nagus-p7x), so a restart continues the day's count instead of
	// starting a fresh allowance. Writes are through; a failed write is
	// logged and the in-memory count stays authoritative.
	State offer.MailState
	Logf  func(string, ...any)

	mu        sync.Mutex
	day       string
	used      int
	exhausted map[string]int64 // scope -> times a reservation was refused
	ok, errs  int64            // vision call results
}

// NewBudget returns a daily budget; perDay <= 0 means DefaultCallsPerDay.
func NewBudget(perDay int, now func() time.Time) *Budget {
	if perDay <= 0 {
		perDay = DefaultCallsPerDay
	}
	if now == nil {
		now = time.Now
	}
	return &Budget{PerDay: perDay, Now: now, exhausted: map[string]int64{}}
}

// sync loads the persisted count when the UTC day changes (and on first
// use). The database is read OUTSIDE mu: a slow database stalls only the
// caller, never every source's reservation. Memory stays authoritative: a
// failed read counts from the in-memory value.
func (b *Budget) sync() {
	d := offer.UsageDay(b.Now())
	b.mu.Lock()
	same := d == b.day
	b.mu.Unlock()
	if same {
		return
	}
	n := 0
	if b.State != nil {
		ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
		v, err := b.State.VisionUsage(ctx, d)
		cancel()
		if err != nil {
			b.logf("promo: reading the day's vision usage: %v (counting from 0)", err)
		} else {
			n = v
		}
	}
	b.mu.Lock()
	if b.day != d {
		b.day, b.used = d, n
	}
	b.mu.Unlock()
}

// rollover is the in-memory part of a day change, for a day that turned
// between sync and the lock. Callers hold mu.
func (b *Budget) rollover() {
	if d := offer.UsageDay(b.Now()); d != b.day {
		b.day, b.used = d, 0
	}
}

// persist adds delta to day's persisted count. Called WITHOUT mu.
func (b *Budget) persist(day string, delta int) {
	if b.State == nil || delta == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	defer cancel()
	if err := b.State.AddVisionUsage(ctx, day, delta); err != nil {
		b.logf("promo: recording vision usage: %v", err)
	}
}

func (b *Budget) logf(f string, a ...any) {
	if b.Logf != nil {
		b.Logf(f, a...)
	}
}

// PollBudget is one source poll's allowance against the shared day.
type PollBudget struct {
	day     *Budget
	PerPoll int
	used    int
	mu      sync.Mutex
}

// NewPoll starts a poll allowance; perPoll <= 0 means DefaultCallsPerPoll.
func (b *Budget) NewPoll(perPoll int) *PollBudget {
	if perPoll <= 0 {
		perPoll = DefaultCallsPerPoll
	}
	return &PollBudget{day: b, PerPoll: perPoll}
}

// Reserve takes n calls from both the poll and the day, all or nothing. A
// refusal names the scope that ran out ("poll" or "day").
func (p *PollBudget) Reserve(n int) (bool, string) {
	b := p.day
	b.sync()
	ok, scope, day := p.reserve(n)
	if ok {
		b.persist(day, n)
	}
	return ok, scope
}

func (p *PollBudget) reserve(n int) (bool, string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.day
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	switch {
	case p.used+n > p.PerPoll:
		b.exhausted["poll"]++
		return false, "poll", ""
	case b.used+n > b.PerDay:
		b.exhausted["day"]++
		return false, "day", ""
	}
	p.used += n
	b.used += n
	return true, "", b.day
}

// Available reports whether n calls could be reserved now, without
// reserving them. A refusal names the scope that ran out and is counted
// (budget exhausted) exactly as a refused Reserve is: an image is refused by
// one or the other, never both.
func (p *PollBudget) Available(n int) (bool, string) {
	p.day.sync()
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.day
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	switch {
	case p.used+n > p.PerPoll:
		b.exhausted["poll"]++
		return false, "poll"
	case b.used+n > b.PerDay:
		b.exhausted["day"]++
		return false, "day"
	}
	return true, ""
}

// Refund returns n reserved calls that were never made.
func (p *PollBudget) Refund(n int) {
	p.mu.Lock()
	b := p.day
	b.mu.Lock()
	p.used = max(0, p.used-n)
	refund := min(n, b.used)
	b.used -= refund
	day := b.day
	b.mu.Unlock()
	p.mu.Unlock()
	b.persist(day, -refund)
}

// Used is the poll's calls so far.
func (p *PollBudget) Used() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.used
}

// record counts one vision call's result.
func (b *Budget) record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.ok++
	} else {
		b.errs++
	}
}

// BudgetStats is a snapshot for metrics.
type BudgetStats struct {
	PerDay, UsedToday int
	Exhausted         map[string]int64
	CallsOK, CallsErr int64
}

// Stats snapshots the budget.
func (b *Budget) Stats() BudgetStats {
	b.sync() // a day change loads the persisted count, never zeroes it
	b.mu.Lock()
	defer b.mu.Unlock()
	ex := map[string]int64{"poll": b.exhausted["poll"], "day": b.exhausted["day"]}
	return BudgetStats{PerDay: b.PerDay, UsedToday: b.used, Exhausted: ex, CallsOK: b.ok, CallsErr: b.errs}
}
