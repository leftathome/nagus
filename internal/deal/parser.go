package deal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/listing"
)

// ParserName is the imapParser value that selects this format.
const ParserName = "deal-jsonl-v1"

// Hub is the state every deal source shares: which source ingests which
// category, who the principals are, and the ledger. One per process.
type Hub struct {
	Ledger *Ledger
	// Sources maps an enabled category to the source id ("imap:<name>") of
	// the deal source that ingests it.
	Sources map[string]string
	// Aliases maps a lower-cased sender address to its principal name.
	Aliases map[string]string
	// Mailbox is the address senders write to (config, never a constant).
	Mailbox string
	// LookbackDays is how far back a poll reads, so how long status lasts.
	LookbackDays int
}

// Principal is who submitted a message: the configured alias of the
// verified sender address, else the address itself.
func (h *Hub) Principal(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if a := strings.ToLower(strings.TrimSpace(h.Aliases[addr])); a != "" {
		return a
	}
	return addr
}

// Recent is the ledger's newest n messages from a principal, given by its
// ALIAS only. An address is never accepted: answering for an address would
// confirm it is on the allowlist (rv35 N3). A sender with no alias has no
// principal lookup; it uses its Message-IDs.
func (h *Hub) Recent(alias string, n int) []MessageView {
	a := strings.ToLower(strings.TrimSpace(alias))
	if a == "" || strings.Contains(a, "@") {
		return nil
	}
	for _, v := range h.Aliases {
		if strings.ToLower(strings.TrimSpace(v)) == a {
			return h.Ledger.Recent(a, n)
		}
	}
	return nil
}

// EnabledCategories lists the categories with a deal source, sorted in
// schema order.
func (h *Hub) EnabledCategories() []string {
	var out []string
	for _, c := range Categories {
		if _, ok := h.Sources[c]; ok {
			out = append(out, c)
		}
	}
	return out
}

// Parser is the imap parser for one deal source (one category). Every deal
// source reads the same messages; each ingests only its category's lines.
type Parser struct {
	hub      *Hub
	category string
	sourceID string
}

var _ imapmail.Parser = (*Parser)(nil)

// NewParser builds the parser for the deal source that ingests category.
func NewParser(h *Hub, category string) (*Parser, error) {
	id, ok := h.Sources[category]
	if !ok {
		return nil, fmt.Errorf("deal: no deal source registered for category %q", category)
	}
	return &Parser{hub: h, category: category, sourceID: id}, nil
}

// Scan limits (rv35 I1): a body is read for at most MaxScanBytes and
// MaxScanLines; past the 50th deal line nothing more is decoded, and the rest
// become ONE too_many_lines entry with a count.
const (
	MaxScanBytes = 256 << 10
	MaxScanLines = 10000
)

// boundary ends the part of a body that is read: an RFC 3676 signature
// delimiter, an Outlook original-message separator or underscore rule, or a
// Gmail or Apple Mail forward marker, quoted or not.
var boundary = regexp.MustCompile(`(?i)^[>\s]*(?:--|-{3,}\s*original message\s*-{3,}|-{3,}\s*forwarded message\s*-{3,}|begin forwarded message:|_{5,})\s*$`)

var (
	// A reply attribution, or its last line when the client wrapped it: a
	// line ENDING in "wrote:" or its German, French, Spanish, Italian, Dutch
	// or Portuguese form. The lines of a wrapped attribution above it are
	// not deal lines, so stopping at its last line is enough.
	attributionRe = regexp.MustCompile(`(?i)^[>\s]*(?:\S.{0,300}\s)?(?:wrote|schrieb|a\s[e\x{e9}]crit|escribi[o\x{f3}]|ha\sscritto|schreef|escreveu)\s?:\s*$`)
	// Gmail in German puts the name after the verb: "Am ... schrieb X <a>:".
	attributionDeRe = regexp.MustCompile(`(?i)^[>\s]*am\s.{1,300}\sschrieb\s.{1,200}:\s*$`)
	// A quoted header block, as Outlook writes above the original: a From
	// line (plain, bold or localized) with another header line within the
	// next three.
	hdrFromRe = regexp.MustCompile(`(?i)^[>\s]*\*{0,2}(?:from|von|de|da|van)\*{0,2}\s?:\*{0,2}\s*\S`)
	hdrNextRe = regexp.MustCompile(`(?i)^[>\s]*\*{0,2}(?:sent|date|to|cc|subject|gesendet|datum|an|betreff|envoy[e\x{e9}]|objet|enviado|para|asunto|verzonden|aan)\*{0,2}\s?:`)
)

// isBoundary reports whether line i starts quoted or forwarded material.
// These patterns are defence in depth: the structural rule is that a plain
// sender's reply (In-Reply-To / References) is not read at all.
func isBoundary(lines []string, i int) bool {
	t := strings.TrimSpace(lines[i])
	if strings.HasPrefix(stripInvisible(t), "{") {
		return false // a deal line is never a boundary
	}
	if boundary.MatchString(t) || attributionRe.MatchString(t) || attributionDeRe.MatchString(t) {
		return true
	}
	if hdrFromRe.MatchString(t) {
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			if hdrNextRe.MatchString(strings.TrimSpace(lines[j])) {
				return true
			}
		}
	}
	return false
}

// stripInvisible removes leading invisible characters (a BOM, zero-width
// space, word joiner, direction marks).
func stripInvisible(t string) string {
	return strings.TrimLeftFunc(t, invisible)
}

// Key is the listing source key of one line of one message from one VERIFIED
// sender: the identity that makes a re-read idempotent. The sender is part of
// it because the sender writes its own Message-ID (rv35 I5).
//
// It is OPAQUE (rv35b NEW-2): an item's source key is returned by get_item
// and GET /item, so it must not carry the sender's address (that would tell
// any MCP caller who is on the allowlist) or the Message-ID (the capability
// for the full status lookup). A domain-separated SHA-256 over
// length-prefixed fields: stable across polls, and no two (sender, id, line)
// triples share one.
func Key(sender, messageID string, line int) string {
	h := sha256.New()
	for _, f := range []string{"nagus.deal/v1 source key", strings.ToLower(sender), messageID, strconv.Itoa(line)} {
		fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return "deal-" + hex.EncodeToString(h.Sum(nil))[:32]
}

// Parse reads one verified message. Refused lines and message-level
// refusals go to the ledger; lines of this parser's category become
// listings.
func (p *Parser) Parse(m imapmail.Message) ([]listing.Raw, error) {
	ref := msgRef{addr: strings.ToLower(m.From), id: m.ID, principal: p.hub.Principal(m.From), received: m.Received}
	led := p.hub.Ledger
	if m.Reply && m.ForwardedBy == "" {
		// A submission is a fresh message (rv35b NEW-6). A reply carries a
		// quoted original that no pattern can delimit for every mail client,
		// so it is not read at all. A forward legitimately carries the same
		// headers, and is read from its marker instead.
		led.refuseMessage(ref, MsgReplyNotAccepted)
		return nil, nil
	}
	if strings.TrimSpace(m.Text) == "" {
		outcome := MsgEmpty
		if strings.TrimSpace(m.HTML) != "" {
			// Deliberate: text derived from HTML depends on the sender's
			// client. Refused whole; see the design doc.
			outcome = MsgNoTextPart
		}
		led.refuseMessage(ref, outcome)
		return nil, nil
	}
	text, truncated := m.Text, false
	if len(text) > MaxScanBytes {
		// Cut at the cap and drop the line the cut went through: a deal
		// line is read whole or not at all.
		text, truncated = text[:MaxScanBytes], true
		if i := strings.LastIndexAny(text, "\r\n"); i >= 0 {
			text = text[:i+1]
		} else {
			text = ""
		}
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > MaxScanLines {
		lines, truncated = lines[:MaxScanLines], true
	}
	start := 0
	if m.ForwardedBy != "" {
		// A household hand-forward: read the forwarded original only, after
		// the marker and the forwarded header block. The marker must be the
		// FIRST boundary in the message and unquoted: one below a reply
		// boundary, a signature or a quote belongs to someone else's text
		// (rv35b NEW-3) and opens nothing.
		start = len(lines)
		for i := range lines {
			if !isBoundary(lines, i) {
				continue
			}
			if isForwardMarker(lines[i]) {
				// Gmail puts the header block straight after the marker,
				// Apple Mail after a blank line.
				start = i + 1
				for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
					start++
				}
				for start < len(lines) && strings.TrimSpace(lines[start]) != "" {
					start++
				}
			}
			break
		}
	}
	var out []listing.Raw
	n, overflowLine, overflow := 0, 0, 0
	reachedEnd := true
	for i := start; i < len(lines); i++ {
		if isBoundary(lines, i) {
			reachedEnd = false
			break
		}
		t := strings.TrimSpace(lines[i])
		bare := stripInvisible(t)
		if !strings.HasPrefix(bare, "{") {
			continue
		}
		n++
		lineNo := i + 1
		if n > MaxLinesPerMessage {
			if overflow == 0 {
				overflowLine = lineNo
			}
			overflow++
			continue
		}
		if bare != t {
			// A byte-order mark or another invisible character before the
			// brace: not JSON, and reported rather than ignored.
			led.reject(ref, lineNo, "", ReasonBadJSON, 0)
			continue
		}
		d, why := Decode([]byte(t))
		if why != ReasonNone {
			led.reject(ref, lineNo, "", why, 0)
			continue
		}
		owner, enabled := p.hub.Sources[d.Category]
		key := Key(m.From, m.ID, lineNo)
		switch {
		case !enabled:
			led.reject(ref, lineNo, d.Category, ReasonCategoryNotEnabled, 0)
		case d.Category != p.category:
			led.placeholder(ref, lineNo, d.Category, owner, key)
		default:
			led.own(ref, lineNo, d.Category, p.sourceID, key)
			out = append(out, d.ToRaw(key, ref.principal))
		}
	}
	if overflow > 0 {
		led.reject(ref, overflowLine, "", ReasonTooManyLines, overflow)
	}
	if truncated && reachedEnd && overflow == 0 {
		// The scan cap, not a boundary, ended the read: say so (rv35b
		// NEW-8) instead of reporting an empty or shorter message.
		led.reject(ref, len(lines)+1, "", ReasonScanTruncated, 0)
	}
	if n == 0 && !(truncated && reachedEnd) {
		led.refuseMessage(ref, MsgEmpty)
	}
	return out, nil
}

// forwardRe is an UNQUOTED forward marker: where a hand-forward's original
// begins.
var forwardRe = regexp.MustCompile(`(?i)^\s*(?:-{3,}\s*forwarded message\s*-{3,}|begin forwarded message:)\s*$`)

func isForwardMarker(ln string) bool { return forwardRe.MatchString(ln) }

// Source wraps a deal source's connector so the ledger knows where each
// fetch begins: a line is settled by the ingest pass that fetched it.
type Source struct {
	Inner listing.Connector
	Hub   *Hub
}

// SourceID is the inner connector's.
func (s *Source) SourceID() string { return s.Inner.SourceID() }

// Fetch opens a ledger generation, then fetches.
func (s *Source) Fetch(ctx context.Context) ([]listing.Raw, error) {
	s.Hub.Ledger.startFetch(s.Inner.SourceID())
	return s.Inner.Fetch(ctx)
}

// FetchComplete forwards the inner connector's completeness, which gates
// offer expiry.
func (s *Source) FetchComplete() bool {
	if rc, ok := s.Inner.(interface{ FetchComplete() bool }); ok {
		return rc.FetchComplete()
	}
	return true
}

// Observer adapts the ledger to the imap connector's skip callback.
func (h *Hub) Observer() func(imapmail.Observation) {
	return func(o imapmail.Observation) {
		h.Ledger.ObserveConnector(o.Key, o.MessageID, connectorOutcome(o.Reason), o.Received)
	}
}

// connectorOutcome maps an imap skip reason onto a message outcome.
func connectorOutcome(r imapmail.SkipReason) string {
	switch r {
	case imapmail.SkipUnknownSender:
		return MsgUnknownSender
	case imapmail.SkipUnverified:
		return MsgUnverified
	case imapmail.SkipTooLarge:
		return MsgTooLarge
	default:
		return MsgInvalid
	}
}

// retainFor is how long the ledger keeps a message after its last poll.
func retainFor(lookbackDays int) time.Duration {
	if lookbackDays <= 0 {
		lookbackDays = imapmail.DefaultLookbackDays
	}
	return time.Duration(lookbackDays+2) * 24 * time.Hour
}

// NewHub builds the shared state for a set of deal sources.
func NewHub(sources map[string]string, aliases map[string]string, mailbox string, lookbackDays int, now func() time.Time) *Hub {
	al := map[string]string{}
	for k, v := range aliases {
		al[strings.ToLower(strings.TrimSpace(k))] = v
	}
	if lookbackDays <= 0 {
		lookbackDays = imapmail.DefaultLookbackDays
	}
	return &Hub{Ledger: NewLedger(retainFor(lookbackDays), now), Sources: sources, Aliases: al,
		Mailbox: mailbox, LookbackDays: lookbackDays}
}
