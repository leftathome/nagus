package ebay

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// DefaultDailyBudget is the per-UTC-day eBay API-call ceiling nagus stays under
// by default. eBay production access is limited to roughly this many calls per
// day (License 2.4/12); exceeding or attempting to circumvent the limit risks
// suspension. Override via Config.DailyBudget.
const DefaultDailyBudget = 5000

// rateLimitRemainingHeader is the response header, if any, by which eBay reports
// how many calls remain in the current window. eBay Browse does not reliably
// send it (the authoritative source is the Analytics getRateLimits API), so
// honoring it is best-effort: when present, it caps calls below the local budget.
const rateLimitRemainingHeader = "X-RateLimit-Remaining"

// ErrBudgetExhausted is returned when the eBay daily API-call budget (or an
// eBay-reported remaining count) is used up. It is a benign back-off signal, not
// a failure: the ingest driver logs it and waits for the next window rather than
// treating it as an error. nagus never circumvents the limit (e.g. by rotating
// Application Keys) -- doing so violates eBay License 2.4.
var ErrBudgetExhausted = errors.New("ebay: daily API call budget exhausted")

// BudgetStats is a point-in-time snapshot of API-call budget usage, for metrics.
type BudgetStats struct {
	Budget    int
	Used      int
	Remaining int
}

// callBudget tracks outbound eBay API calls against a per-UTC-day ceiling. It is
// safe for concurrent use. A remaining count learned from a response header
// (hdrRemain >= 0) further caps calls below the local budget.
type callBudget struct {
	now    func() time.Time
	budget int

	mu        sync.Mutex
	day       int // UTC day identity of the current window
	used      int
	hdrRemain int // last eBay-reported remaining; -1 == unknown
}

func newCallBudget(budget int, now func() time.Time) *callBudget {
	if budget <= 0 {
		budget = DefaultDailyBudget
	}
	if now == nil {
		now = time.Now
	}
	return &callBudget{now: now, budget: budget, hdrRemain: -1}
}

// dayKey collapses a time to a UTC-day identity (stable within a day, distinct
// across days), used to roll the counter at UTC midnight.
func dayKey(t time.Time) int {
	u := t.UTC()
	return u.Year()*1000 + u.YearDay()
}

// rollLocked resets the counter when the UTC day has advanced. Caller holds mu.
func (b *callBudget) rollLocked() {
	if d := dayKey(b.now()); d != b.day {
		b.day = d
		b.used = 0
		b.hdrRemain = -1
	}
}

// reserve accounts for one outbound API call, rolling the counter at UTC
// midnight. It returns ErrBudgetExhausted (reserving nothing) when the local
// budget or an eBay-reported remaining count is used up.
func (b *callBudget) reserve() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if b.used >= b.budget || b.hdrRemain == 0 {
		return ErrBudgetExhausted
	}
	b.used++
	if b.hdrRemain > 0 {
		b.hdrRemain--
	}
	return nil
}

// observeRemaining records an eBay-reported remaining-call count from a response
// header so subsequent reserves honor eBay's authoritative number.
func (b *callBudget) observeRemaining(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if n < 0 {
		n = 0
	}
	b.hdrRemain = n
}

// stats returns the current budget accounting. Remaining is the smaller of the
// local headroom and any eBay-reported remaining count.
func (b *callBudget) stats() BudgetStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	rem := b.budget - b.used
	if rem < 0 {
		rem = 0
	}
	if b.hdrRemain >= 0 && b.hdrRemain < rem {
		rem = b.hdrRemain
	}
	return BudgetStats{Budget: b.budget, Used: b.used, Remaining: rem}
}

// observeRateHeaders parses eBay's remaining-call header from h, if present and
// well-formed, and records it. Malformed or absent headers are ignored.
func (b *callBudget) observeRateHeaders(h http.Header) {
	v := h.Get(rateLimitRemainingHeader)
	if v == "" {
		return
	}
	if n, err := strconv.Atoi(v); err == nil {
		b.observeRemaining(n)
	}
}
