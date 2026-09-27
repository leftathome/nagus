package deal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/sanitize"
)

// Outcome is where one line, or one message, stands.
type Outcome string

const (
	OutcomePending  Outcome = "pending"
	OutcomeAccepted Outcome = "accepted"
	OutcomeRejected Outcome = "rejected"
)

// Message outcomes: the bounded label set of nagus_deal_submissions_total.
// The first three are derived from a message's lines once all are final.
const (
	MsgAccepted      = "accepted" // every deal line accepted
	MsgPartial       = "partial"  // some accepted, some rejected
	MsgRejected      = "rejected" // had deal lines, none accepted
	MsgEmpty         = "empty"    // a verified message with no deal line
	MsgNoTextPart    = "no_text_part"
	MsgUnknownSender = "unknown_sender"
	MsgUnverified    = "unverified"
	MsgTooLarge      = "too_large"
	MsgInvalid       = "invalid"
)

// MessageOutcomes is every message outcome, in metric order.
var MessageOutcomes = []string{MsgAccepted, MsgPartial, MsgRejected, MsgEmpty, MsgNoTextPart,
	MsgUnknownSender, MsgUnverified, MsgTooLarge, MsgInvalid}

type lineState struct {
	line     int
	outcome  Outcome
	reason   Reason
	category string
	sourceID string // the deal source that owns (ingests) the line
	key      string // the listing source key
	gen      int64  // the owning source's fetch generation; -1 = placeholder
	counted  bool
}

type msgState struct {
	id        string
	principal string
	received  time.Time
	refusal   string // a message-level outcome when the message had no lines to judge
	lines     map[int]*lineState
	lastSeen  time.Time
	counted   bool
}

// Ledger is the in-memory status of recent submissions and the source of the
// submission counters. The imap connector re-reads the whole lookback window
// on every poll, so the ledger is rebuilt continuously and always covers
// exactly that window; it needs no table. Safe for concurrent use: every
// deal source shares one ledger.
type Ledger struct {
	mu     sync.Mutex
	msgs   map[string]*msgState
	gen    map[string]int64     // sourceID -> current fetch generation
	once   map[string]time.Time // connector-level outcome keys already counted
	subs   map[string]int64     // message outcome -> count
	lines  map[[2]string]int64  // {outcome, reason} -> count
	now    func() time.Time
	retain time.Duration
}

// NewLedger returns an empty ledger. retain is how long a message is kept
// after it was last seen by a poll (the lookback window plus slack); 0 means
// 30 days.
func NewLedger(retain time.Duration, now func() time.Time) *Ledger {
	if retain <= 0 {
		retain = 30 * 24 * time.Hour
	}
	if now == nil {
		now = time.Now
	}
	return &Ledger{
		msgs: map[string]*msgState{}, gen: map[string]int64{},
		once: map[string]time.Time{}, subs: map[string]int64{}, lines: map[[2]string]int64{},
		now: now, retain: retain,
	}
}

// startFetch opens a new fetch generation for a deal source.
func (l *Ledger) startFetch(sourceID string) {
	l.mu.Lock()
	l.gen[sourceID]++
	l.mu.Unlock()
}

// message returns the message's state, creating it. Callers hold mu.
func (l *Ledger) message(id, principal string, received time.Time) *msgState {
	m, ok := l.msgs[id]
	if !ok {
		if received.IsZero() {
			received = l.now()
		}
		m = &msgState{id: id, received: received, lines: map[int]*lineState{}}
		l.msgs[id] = m
	}
	if principal != "" {
		m.principal = principal
	}
	m.lastSeen = l.now()
	return m
}

// refuseMessage records a message-level outcome for a message nagus read but
// could not take lines from (empty, no_text_part).
func (l *Ledger) refuseMessage(id, principal string, received time.Time, outcome string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.message(id, principal, received)
	m.refusal = outcome
	l.countMessage(m)
}

// reject records a final parser refusal for one line. Every deal source reads
// the same message and reaches the same verdict; the line is counted once.
func (l *Ledger) reject(id, principal string, received time.Time, line int, category string, r Reason) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.message(id, principal, received)
	if ls, ok := m.lines[line]; ok && ls.outcome == OutcomeRejected && ls.reason == r {
		return
	}
	ls := &lineState{line: line, outcome: OutcomeRejected, reason: r, category: category}
	m.lines[line] = ls
	l.countLine(ls)
	l.countMessage(m)
}

// own records a line this source will ingest in its current fetch. An
// accepted line stays accepted (a re-read is not news); anything else is
// pending until ApplyIngest decides it.
func (l *Ledger) own(id, principal string, received time.Time, line int, category, sourceID, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.message(id, principal, received)
	gen := l.gen[sourceID]
	if ls, ok := m.lines[line]; ok && ls.sourceID == sourceID && ls.key == key && ls.outcome == OutcomeAccepted {
		ls.gen = gen
		return
	}
	ls := &lineState{line: line, outcome: OutcomePending, category: category, sourceID: sourceID, key: key, gen: gen}
	if old, ok := m.lines[line]; ok {
		ls.counted = old.counted && old.sourceID == sourceID
	}
	m.lines[line] = ls
}

// placeholder marks a line another deal source owns, so the message is not
// judged final before that source has read it. It never overwrites.
func (l *Ledger) placeholder(id, principal string, received time.Time, line int, category, sourceID, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.message(id, principal, received)
	if _, ok := m.lines[line]; ok {
		return
	}
	m.lines[line] = &lineState{line: line, outcome: OutcomePending, category: category, sourceID: sourceID, key: key, gen: -1}
}

// ObserveConnector counts a message the connector skipped before any parser
// saw it, once per key (a Message-ID, or a UID key for mail that was never
// fetched). outcome is one of the message outcomes. A message with a
// Message-ID is also recorded, so its status can be looked up.
func (l *Ledger) ObserveConnector(key, messageID, outcome string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if messageID != "" {
		m := l.message(messageID, "", time.Time{})
		m.refusal = outcome
	}
	if _, ok := l.once[key]; ok {
		l.once[key] = now
		return
	}
	l.once[key] = now
	l.subs[outcome]++
}

// ApplyIngest settles this source's lines from one ingest pass: a skip at the
// gate, the extractor or the store becomes the line's reason, and every other
// line the source fetched this pass is accepted. Transient failures leave the
// line pending with a reason; the next poll retries it.
func (l *Ledger) ApplyIngest(sourceID string, res pipeline.IngestResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	gen := l.gen[sourceID]
	skipped := map[string]Reason{}
	for _, s := range res.Skips {
		if r := skipReason(s); r != ReasonNone {
			// The first failing stage decides; an offer-store failure is
			// overridden by a later item-path verdict.
			if prev, ok := skipped[s.SourceKey]; !ok || prev == ReasonOfferFailed {
				skipped[s.SourceKey] = r
			}
		}
	}
	touched := map[*msgState]bool{}
	for _, m := range l.msgs {
		for _, ls := range m.lines {
			if ls.sourceID != sourceID || ls.gen != gen || ls.outcome == OutcomeRejected {
				continue
			}
			touched[m] = true
			r, failed := skipped[ls.key]
			switch {
			case !failed:
				ls.outcome, ls.reason = OutcomeAccepted, ReasonNone
			case isFinal(r):
				ls.outcome, ls.reason = OutcomeRejected, r
			default:
				ls.outcome, ls.reason = OutcomePending, r
			}
			if ls.outcome != OutcomePending {
				l.countLine(ls)
			}
		}
	}
	for m := range touched {
		l.countMessage(m)
	}
	l.prune()
}

// skipReason maps an ingest skip onto a line reason.
func skipReason(s pipeline.Skip) Reason {
	switch s.Stage {
	case "sanitize":
		if errors.Is(s.Err, sanitize.ErrQuarantined) {
			return ReasonGateRefused
		}
		return ReasonGateUnavailable
	case "extract":
		if errors.Is(s.Err, listing.ErrNotInCategory) {
			return ReasonNotInCategory
		}
		return ReasonExtractFailed
	case "store":
		return ReasonStoreFailed
	case "offer":
		return ReasonOfferFailed
	}
	return ReasonNone
}

func isFinal(r Reason) bool {
	for _, lr := range LineReasons {
		if lr.Code == r {
			return lr.Final
		}
	}
	return false
}

// countLine counts a line at its first final outcome. Callers hold mu.
func (l *Ledger) countLine(ls *lineState) {
	if ls.counted || ls.outcome == OutcomePending {
		return
	}
	ls.counted = true
	reason := string(ls.reason)
	if reason == "" {
		reason = "none"
	}
	l.lines[[2]string{string(ls.outcome), reason}]++
}

// countMessage counts a message once, when it has a final outcome. Callers
// hold mu.
func (l *Ledger) countMessage(m *msgState) {
	if m.counted {
		return
	}
	o := m.outcome()
	if o == string(OutcomePending) {
		return
	}
	m.counted = true
	l.subs[o]++
}

// outcome is the message's overall outcome. Callers hold mu.
func (m *msgState) outcome() string {
	if len(m.lines) == 0 {
		if m.refusal != "" {
			return m.refusal
		}
		return string(OutcomePending)
	}
	acc, rej := 0, 0
	for _, ls := range m.lines {
		switch ls.outcome {
		case OutcomePending:
			return string(OutcomePending)
		case OutcomeAccepted:
			acc++
		case OutcomeRejected:
			rej++
		}
	}
	switch {
	case rej == 0:
		return MsgAccepted
	case acc == 0:
		return MsgRejected
	default:
		return MsgPartial
	}
}

// prune drops what no poll has seen for the retention period: the message is
// out of the lookback window and will not be read again. Callers hold mu.
func (l *Ledger) prune() {
	cut := l.now().Add(-l.retain)
	for id, m := range l.msgs {
		if m.lastSeen.Before(cut) {
			delete(l.msgs, id)
		}
	}
	for k, at := range l.once {
		if at.Before(cut) {
			delete(l.once, k)
		}
	}
}

// LineView is one line's status. It never carries line content: only the
// line number, the outcome, a reason code, the category enum and ids.
type LineView struct {
	Line     int    `json:"line"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason,omitempty"`
	Category string `json:"category,omitempty"`
	// OfferID is set for an accepted line (message-id lookups only). It is
	// also the item id, so get_item takes it.
	OfferID string `json:"offer_id,omitempty"`
	// Resolution and ProductID are quark's answer for the offer, filled by
	// the caller from the offer store.
	Resolution string `json:"resolution,omitempty"`
	ProductID  string `json:"product_id,omitempty"`
}

// Counts summarizes a message's lines.
type Counts struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Pending  int `json:"pending"`
}

// MessageView is one message's status.
type MessageView struct {
	MessageID string     `json:"message_id,omitempty"`
	Received  time.Time  `json:"received"`
	Outcome   string     `json:"outcome"`
	Counts    Counts     `json:"counts"`
	Lines     []LineView `json:"lines"`
	principal string
}

func (l *Ledger) view(m *msgState, withIDs bool) MessageView {
	v := MessageView{Received: m.received, Outcome: m.outcome(), Lines: []LineView{}, principal: m.principal}
	if withIDs {
		v.MessageID = m.id
	}
	nums := make([]int, 0, len(m.lines))
	for n := range m.lines {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		ls := m.lines[n]
		lv := LineView{Line: ls.line, Outcome: string(ls.outcome), Reason: string(ls.reason), Category: ls.category}
		switch ls.outcome {
		case OutcomeAccepted:
			v.Counts.Accepted++
			if withIDs {
				lv.OfferID = offer.DeterministicID(ls.sourceID, ls.key)
			}
		case OutcomeRejected:
			v.Counts.Rejected++
		default:
			v.Counts.Pending++
		}
		v.Lines = append(v.Lines, lv)
	}
	return v
}

// Lookup returns a message's status by Message-ID (angle brackets optional),
// with offer ids for accepted lines.
func (l *Ledger) Lookup(messageID string) (MessageView, bool) {
	id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(messageID), "<"), ">")
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.msgs[id]
	if !ok {
		return MessageView{}, false
	}
	return l.view(m, true), true
}

// Recent returns the newest n messages from a principal, WITHOUT message ids
// or offer ids: knowing a principal's name is weaker than holding a
// Message-ID, so it unlocks only outcomes and reason codes.
func (l *Ledger) Recent(principal string, n int) []MessageView {
	p := strings.ToLower(strings.TrimSpace(principal))
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []MessageView
	for _, m := range l.msgs {
		if p != "" && m.principal == p {
			out = append(out, l.view(m, false))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Received.After(out[j].Received) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// WriteMetrics renders the submission counters with every series of the
// bounded label sets present, zeros included.
func (l *Ledger) WriteMetrics(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(w, "# HELP nagus_deal_submissions_total Messages on the deals mailbox read as nagus.deal/v1 submissions, by outcome, counted once per message per process.\n")
	fmt.Fprintf(w, "# TYPE nagus_deal_submissions_total counter\n")
	for _, o := range MessageOutcomes {
		fmt.Fprintf(w, "nagus_deal_submissions_total{outcome=%q} %d\n", o, l.subs[o])
	}
	fmt.Fprintf(w, "# HELP nagus_deal_submissions_lines_total nagus.deal/v1 deal lines at their first final outcome, by outcome and reason code.\n")
	fmt.Fprintf(w, "# TYPE nagus_deal_submissions_lines_total counter\n")
	fmt.Fprintf(w, "nagus_deal_submissions_lines_total{outcome=\"accepted\",reason=\"none\"} %d\n", l.lines[[2]string{"accepted", "none"}])
	for _, lr := range LineReasons {
		if !lr.Final {
			continue
		}
		fmt.Fprintf(w, "nagus_deal_submissions_lines_total{outcome=\"rejected\",reason=%q} %d\n", lr.Code, l.lines[[2]string{"rejected", string(lr.Code)}])
	}
}

// AfterIngest is the pipeline.Ingester hook for a deal source.
func (l *Ledger) AfterIngest(sourceID string) func(context.Context, pipeline.IngestResult) {
	return func(_ context.Context, res pipeline.IngestResult) { l.ApplyIngest(sourceID, res) }
}
