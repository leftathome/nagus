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
	MsgAccepted   = "accepted" // every deal line accepted
	MsgPartial    = "partial"  // some accepted, some rejected
	MsgRejected   = "rejected" // had deal lines, none accepted
	MsgEmpty      = "empty"    // a verified message with no deal line
	MsgNoTextPart = "no_text_part"
	// MsgReplyNotAccepted: a plain sender's message carried In-Reply-To or
	// References. A submission is a fresh message; replies are not read.
	MsgReplyNotAccepted = "reply_not_accepted"
	MsgUnknownSender    = "unknown_sender"
	MsgUnverified       = "unverified"
	MsgTooLarge         = "too_large"
	MsgInvalid          = "invalid"
)

// MessageOutcomes is every message outcome, in metric order.
var MessageOutcomes = []string{MsgAccepted, MsgPartial, MsgRejected, MsgEmpty, MsgNoTextPart,
	MsgReplyNotAccepted, MsgUnknownSender, MsgUnverified, MsgTooLarge, MsgInvalid}

// MessageOutcomeMeanings explains the outcomes a SENDER can see in status
// (the connector-level ones -- unknown_sender, unverified, too_large, invalid
// -- are counted but never reported for a message).
var MessageOutcomeMeanings = map[string]string{
	string(OutcomePending): "not every deal line has been processed yet; ask again after the next poll",
	MsgAccepted:            "every deal line was accepted",
	MsgPartial:             "some deal lines were accepted and some rejected; see each line's reason",
	MsgRejected:            "the message had deal lines and none was accepted",
	MsgEmpty:               "no line starting with { was found above the first signature, quote or forward",
	MsgNoTextPart:          "the message had no text/plain part (HTML only); send plain text",
	MsgReplyNotAccepted:    "the message is a reply (In-Reply-To or References); send a new message, replies are not read",
}

// MaxLedgerMessages bounds the ledger: past it, the message no poll has seen
// for longest is dropped (rv35 I1). Far above what a household sends in a
// lookback window.
const MaxLedgerMessages = 5000

// MaxObserved bounds the connector-level observations (unknown senders,
// unverified mail) remembered for once-only counting (rv35b NEW-9).
const MaxObserved = 20000

// UnverifiedRecentWindow is the window of nagus_deal_unverified_last_24h.
const UnverifiedRecentWindow = 24 * time.Hour

// msgRef identifies one message: the VERIFIED sender address and the
// Message-ID. The sender writes its own Message-ID, so the id alone keys
// nothing (rv35 I5): two senders can never share a ledger entry or a key.
type msgRef struct {
	addr      string // verified sender address, lower-cased
	id        string // Message-ID
	principal string // alias, or addr
	received  time.Time
}

func (r msgRef) key() string { return r.addr + "\x00" + r.id }

type lineState struct {
	line     int
	outcome  Outcome
	reason   Reason
	count    int // too_many_lines summary: deal lines not read
	category string
	sourceID string // the deal source that owns (ingests) the line
	key      string // the listing source key
	gen      int64  // the owning source's fetch generation; -1 = placeholder
	counted  bool
}

type msgState struct {
	ref      msgRef
	refusal  string // a message-level outcome when the message had no lines to judge
	lines    map[int]*lineState
	lastSeen time.Time
	counted  bool
}

type observed struct {
	outcome  string
	received time.Time
	lastSeen time.Time
}

// Ledger is the in-memory status of recent submissions and the source of the
// submission counters. The imap connector re-reads the whole lookback window
// on every poll, so the ledger is rebuilt continuously and always covers
// exactly that window; it needs no table. Safe for concurrent use: every
// deal source shares one ledger. Bounded: at most MaxLedgerMessages
// messages of at most MaxLinesPerMessage+1 lines each.
type Ledger struct {
	mu     sync.Mutex
	msgs   map[string]*msgState // msgRef.key() -> state
	gen    map[string]int64     // sourceID -> current fetch generation
	once   map[string]*observed // connector-level outcomes, by key
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
		once: map[string]*observed{}, subs: map[string]int64{}, lines: map[[2]string]int64{},
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
func (l *Ledger) message(r msgRef) *msgState {
	k := r.key()
	m, ok := l.msgs[k]
	if !ok {
		if len(l.msgs) >= MaxLedgerMessages {
			l.evictOldest()
		}
		if r.received.IsZero() {
			r.received = l.now()
		}
		m = &msgState{ref: r, lines: map[int]*lineState{}}
		l.msgs[k] = m
	}
	if r.principal != "" {
		m.ref.principal = r.principal
	}
	m.lastSeen = l.now()
	return m
}

func (l *Ledger) evictOldest() {
	var oldest string
	var at time.Time
	for k, m := range l.msgs {
		if oldest == "" || m.lastSeen.Before(at) {
			oldest, at = k, m.lastSeen
		}
	}
	delete(l.msgs, oldest)
}

// refuseMessage records a message-level outcome for a message nagus read but
// could not take lines from (empty, no_text_part).
func (l *Ledger) refuseMessage(r msgRef, outcome string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.message(r)
	m.refusal = outcome
	l.countMessage(m)
}

// reject records a final parser refusal for one line. Every deal source reads
// the same message and reaches the same verdict; the line is counted once.
// count is the number of unread lines on a too_many_lines summary.
func (l *Ledger) reject(r msgRef, line int, category string, why Reason, count int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.message(r)
	if ls, ok := m.lines[line]; ok && ls.outcome == OutcomeRejected && ls.reason == why {
		ls.count = count
		return
	}
	ls := &lineState{line: line, outcome: OutcomeRejected, reason: why, category: category, count: count}
	m.lines[line] = ls
	l.countLine(ls)
	l.countMessage(m)
}

// own records a line this source will ingest in its current fetch. An
// accepted line stays accepted (a re-read is not news); anything else is
// pending until ApplyIngest decides it.
func (l *Ledger) own(r msgRef, line int, category, sourceID, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.message(r)
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
func (l *Ledger) placeholder(r msgRef, line int, category, sourceID, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.message(r)
	if _, ok := m.lines[line]; ok {
		return
	}
	m.lines[line] = &lineState{line: line, outcome: OutcomePending, category: category, sourceID: sourceID, key: key, gen: -1}
}

// ObserveConnector counts a message the connector skipped before any parser
// saw it, once per key (a Message-ID, or a UID key for mail that was never
// fetched). outcome is one of the message outcomes; received is the server's
// arrival time (zero when unknown). Nothing about the message is kept for
// status: an unverified message answers exactly like one nagus never saw, so
// the status tool is no allowlist oracle (rv35 N3).
func (l *Ledger) ObserveConnector(key, _ /*messageID*/, outcome string, received time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if o, ok := l.once[key]; ok {
		o.lastSeen = now
		// The same key with a LATER arrival is a newer message (a caller
		// keying by something the sender controls must not hide it from the
		// gauge); it is still counted once.
		if received.After(o.received) {
			o.received = received
		}
		return
	}
	if received.IsZero() {
		received = now
	}
	if len(l.once) >= MaxObserved {
		l.evictObserved()
	}
	l.once[key] = &observed{outcome: outcome, received: received, lastSeen: now}
	l.subs[outcome]++
}

// evictObserved drops the tenth of the observations that ARRIVED longest ago
// (rv35b NEW-9). An evicted message still in the lookback window is counted
// again on the next poll; the bound matters more than that. Callers hold mu.
//
// Unverified observations go LAST (rv35c R3): they are what the alert gauge
// reads, and mail from unknown senders carries no arrival time (it is never
// fetched), so by arrival alone a flood of it would push the unverified
// entries out and make the gauge dip mid-poll.
func (l *Ledger) evictObserved() {
	type entry struct {
		key        string
		unverified bool
		received   time.Time
	}
	all := make([]entry, 0, len(l.once))
	for k, o := range l.once {
		all = append(all, entry{k, o.outcome == MsgUnverified, o.received})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].unverified != all[j].unverified {
			return !all[i].unverified
		}
		if !all[i].received.Equal(all[j].received) {
			return all[i].received.Before(all[j].received)
		}
		return all[i].key < all[j].key
	})
	for _, e := range all[:max(1, len(all)/10)] {
		delete(l.once, e.key)
	}
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
	for k, m := range l.msgs {
		if m.lastSeen.Before(cut) {
			delete(l.msgs, k)
		}
	}
	for k, o := range l.once {
		if o.lastSeen.Before(cut) {
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
	// Count is, on a too_many_lines summary entry, how many deal lines past
	// the limit were not read.
	Count int `json:"count,omitempty"`
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
	MessageID string `json:"message_id,omitempty"`
	// Received is when the mail server received it (IMAP INTERNALDATE).
	Received time.Time  `json:"received"`
	Outcome  string     `json:"outcome"`
	Counts   Counts     `json:"counts"`
	Lines    []LineView `json:"lines"`
}

func (l *Ledger) view(m *msgState, withIDs bool) MessageView {
	v := MessageView{Received: m.ref.received, Outcome: m.outcome(), Lines: []LineView{}}
	if withIDs {
		v.MessageID = m.ref.id
	}
	nums := make([]int, 0, len(m.lines))
	for n := range m.lines {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		ls := m.lines[n]
		lv := LineView{Line: ls.line, Outcome: string(ls.outcome), Reason: string(ls.reason), Category: ls.category, Count: ls.count}
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

// normID strips angle brackets and space from a Message-ID.
func normID(id string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(id), "<"), ">")
}

// LookupAll returns every verified message with this Message-ID (angle
// brackets optional), newest first, with offer ids for accepted lines. More
// than one only when two allowlisted senders used the same id: each keeps
// its own entry.
func (l *Ledger) LookupAll(messageID string) []MessageView {
	id := normID(messageID)
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []MessageView
	for _, m := range l.msgs {
		if m.ref.id == id {
			out = append(out, l.view(m, true))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Received.After(out[j].Received) })
	return out
}

// Lookup is LookupAll's newest match.
func (l *Ledger) Lookup(messageID string) (MessageView, bool) {
	all := l.LookupAll(messageID)
	if len(all) == 0 {
		return MessageView{}, false
	}
	return all[0], true
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
		if p != "" && m.ref.principal == p {
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
// bounded label sets present, zeros included, and the unverified gauge.
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
	// A gauge over ARRIVAL time, so a restart that recounts the lookback
	// window cannot re-fire the alert for old mail (rv35 N7).
	cut := l.now().Add(-UnverifiedRecentWindow)
	recent := 0
	for _, o := range l.once {
		if o.outcome == MsgUnverified && o.received.After(cut) {
			recent++
		}
	}
	fmt.Fprintf(w, "# HELP nagus_deal_unverified_last_24h Messages From an allowlisted address without a DKIM pass that ARRIVED in the last 24h (possible spoofs).\n")
	fmt.Fprintf(w, "# TYPE nagus_deal_unverified_last_24h gauge\n")
	fmt.Fprintf(w, "nagus_deal_unverified_last_24h %d\n", recent)
}

// AfterIngest is the pipeline.Ingester hook for a deal source.
func (l *Ledger) AfterIngest(sourceID string) func(context.Context, pipeline.IngestResult) {
	return func(_ context.Context, res pipeline.IngestResult) { l.ApplyIngest(sourceID, res) }
}
