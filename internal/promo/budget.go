package promo

import (
	"sync"
	"time"
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

// rollover resets the day's count at UTC midnight. Callers hold mu.
func (b *Budget) rollover() {
	d := b.Now().UTC().Format("2006-01-02")
	if d != b.day {
		b.day, b.used = d, 0
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
	p.used += n
	b.used += n
	return true, ""
}

// Available reports whether n calls could be reserved now, without
// reserving them. A refusal names the scope that ran out and is counted
// (budget exhausted) exactly as a refused Reserve is: an image is refused by
// one or the other, never both.
func (p *PollBudget) Available(n int) (bool, string) {
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
	defer p.mu.Unlock()
	b := p.day
	b.mu.Lock()
	defer b.mu.Unlock()
	p.used = max(0, p.used-n)
	b.used = max(0, b.used-n)
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
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	ex := map[string]int64{"poll": b.exhausted["poll"], "day": b.exhausted["day"]}
	return BudgetStats{PerDay: b.PerDay, UsedToday: b.used, Exhausted: ex, CallsOK: b.ok, CallsErr: b.errs}
}
