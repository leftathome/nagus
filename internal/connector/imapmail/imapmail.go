// Package imapmail reads offers from a mailbox: catalogs, flyers and
// daily-offer emails sent to deals@totally.apocryph.al (nagus-239).
//
// Some sellers publish offers ONLY by email -- flash sites (Last Bottle,
// WTSO) and Seattle's email-offer merchants (Full Pull, Garagiste) have no
// pollable catalogue -- and producers announce releases to their mailing
// lists first. A mailbox is also an OPEN channel: anyone can send it
// anything. So this connector trusts nothing about a message except what the
// receiving mail server verified:
//
//   - One source per SENDER. A source declares one From address and reads
//     only that sender's mail; everything else in the mailbox is ignored.
//     Legality (wineChannel/origin) is per seller, so it is per source.
//   - A From header is trivially forged. A message is accepted only with a
//     DKIM pass for the sender's domain (or its parent, relaxed alignment),
//     established either way (nagus-zsi):
//     1. nagus verifies a DKIM-Signature itself (public key from DNS). This
//     is the path that works on deals@: ForwardEmail writes NO
//     Authentication-Results header, only ARC sets, so path 2 alone
//     rejected every message. Filter-forwarding (Gmail) keeps the
//     original signature intact. Signatures with l= (a partially signed
//     body) and rsa-sha1 are refused by the verifier.
//     2. OPT-IN (Config.TrustAuthResults, off by default; rv35 C1): the
//     TOPMOST Authentication-Results header -- the one a receiving MX
//     prepends on arrival -- comes from a trusted authserv-id and reports
//     dkim=pass. ForwardEmail writes NO A-R header, so on deals@ the
//     topmost one is whatever the SENDER wrote: trusting it accepted
//     unsigned forgeries. Enable only behind an MX that prepends A-R to
//     every message. Deeper A-R headers are never consulted.
//     A message must have exactly one From, and at most one Sender,
//     Subject and Message-ID (rv35 C2: DKIM verifies the bottom-most From
//     its h= covers, so a second From prepended above a genuine signature
//     would otherwise be the one credited).
//     ARC sets are deliberately NOT trusted without verifying the seal chain:
//     lower instances are sender-writable.
//   - Forwarded mail (nagus-zsi, option 3). A source may name Forwarders --
//     the household's own mailboxes, from config only. A message From a
//     forwarder, with a DKIM pass for the FORWARDER's domain, whose forwarded
//     original names this source's sender, is read as that sender's mail: a
//     person forwarding a newsletter by hand vouches for it. The message is
//     CREDITED to the forwarder (Message.From), the only address DKIM
//     verified; the original named in the forwarded text is a claim
//     (Message.ForwardedFrom, aspect mail_forwarded_from), rv35 N1.
//   - A source may instead name a LIST of senders (Senders): the deal
//     submission format (nagus.deal/v1, package deal) is written by the
//     household's own humans and agents, each with their own address. Every
//     sender is held to the same DKIM rule for ITS domain, and a message is
//     credited to the sender that actually sent it. Such a source may also
//     count the mail it ignores (CountIgnored) without reading it.
//   - IMAP SEARCH FROM is a substring match, so each candidate's envelope
//     From is checked exactly before its body is fetched (rv35 N2).
//   - Read-only and stateless: the mailbox is EXAMINEd, never modified; each
//     poll searches the sender's mail over a lookback window, and the
//     Message-ID keys every offer, so a re-read is idempotent.
//
// Turning a verified message into offers is the sender's Parser: mail has no
// common schema, so each sender's layout is parsed by code written from a
// real captured email of that sender. The text then crosses the glovebox
// sanitize gate like every other source (nagus-9ib).
package imapmail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	netmail "net/mail"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	_ "github.com/emersion/go-message/charset" // non-UTF-8 bodies
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-msgauth/dkim"
	"golang.org/x/net/publicsuffix"

	"github.com/leftathome/nagus/internal/arc"
	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the connector family; a configured source is "imap:<Name>".
const SourceID = "imap"

// DefaultLookbackDays is the window searched each poll.
const DefaultLookbackDays = 14

// DefaultMaxBytes bounds one message; a larger one is skipped, not truncated.
const DefaultMaxBytes = 4 << 20

// DefaultDNSTimeout bounds one DKIM key lookup.
const DefaultDNSTimeout = 5 * time.Second

// DefaultTrustedAuthServ is the receiving MX whose Authentication-Results
// header is trusted: ForwardEmail holds the MX for apocryph.al.
var DefaultTrustedAuthServ = []string{"forwardemail.net"}

// Message is one verified email, handed to the sender's Parser.
type Message struct {
	ID string // Message-ID, without angle brackets
	// From is the verified sender address, lower-cased: the source's From,
	// or, on a multi-sender source, whichever of its Senders sent it; for a
	// hand-forward, the forwarder (the address DKIM verified).
	From string
	// ForwardedBy is the forwarder address when this is a hand-forwarded copy
	// of the sender's mail; empty for mail the sender sent directly.
	ForwardedBy string
	// ForwardedFrom is the original sender NAMED inside a hand-forward's
	// text. It is unauthenticated (anyone can type a From: line) and is
	// recorded as a claim only; the credit goes to From, the forwarder.
	ForwardedFrom string
	// Received is when the mail server received the message (IMAP
	// INTERNALDATE), which the sender cannot set. Date is the sender's claim.
	Received time.Time
	// Reply reports an In-Reply-To or References header: the message is a
	// reply or a forward, not a fresh one.
	Reply   bool
	Subject string
	Date    time.Time
	Text    string // the text/plain part, if any (format=flowed is un-flowed)
	HTML    string // the text/html part, if any
}

// SkipReason is why the connector skipped a message before any parser saw
// it. A closed set, for counting.
type SkipReason string

const (
	SkipUnknownSender SkipReason = "unknown_sender"
	SkipUnverified    SkipReason = "unverified"
	SkipTooLarge      SkipReason = "too_large"
	SkipInvalid       SkipReason = "invalid"
	SkipParseError    SkipReason = "parse_error"
)

// Observation reports one skipped message to Config.Observe. Key identifies
// the message stably across polls: its Message-ID when known, else a UID
// key. A poll re-reads its whole window, so observers see the same message
// on every poll and must count by Key.
type Observation struct {
	Key       string
	MessageID string // "" when unknown (never fetched, or unparseable)
	Received  time.Time
	Reason    SkipReason
}

// Parser turns one sender's message into offers. Each implementation is
// written from a real captured email of that sender.
type Parser interface {
	Parse(m Message) ([]listing.Raw, error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Parser{}
)

// Register makes a parser available to configuration by name.
func Register(name string, p Parser) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = p
}

// Lookup returns a registered parser, or an error naming the ones that exist.
func Lookup(name string) (Parser, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if p, ok := registry[name]; ok {
		return p, nil
	}
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("imapmail: no parser named %q (registered: %v); each sender's parser is written from a real captured email", name, names)
}

// Config configures one sender on the shared mailbox.
type Config struct {
	Name string
	// Connection. TLS is "implicit" (993, the default), "starttls", or "none"
	// (tests only).
	Host, Port, Username, Password, TLS string
	// Mailbox defaults to INBOX.
	Mailbox string
	// From is the one sender this source reads. Required unless Senders is
	// set.
	From string
	// Senders is a sender ALLOWLIST: more addresses this source reads, each
	// DKIM-verified for its own domain. A message is credited to the sender
	// that sent it (Message.From).
	Senders []string
	// CountIgnored makes each poll also count the mail in the window from
	// anyone else, by UID only (never fetched), reported to Observe as
	// SkipUnknownSender.
	CountIgnored bool
	// Observe, when set, is told about every message skipped before
	// parsing (see Observation).
	Observe func(Observation)
	// DKIMDomain is the domain From's DKIM signature must be aligned to;
	// empty = the domain of From. Senders always use their own domain.
	DKIMDomain string
	// TrustAuthResults turns on the Authentication-Results path. OFF by
	// default (rv35 C1): ForwardEmail, which holds our MX, writes no A-R
	// header, so the "topmost" one is always the SENDER's own forgery. Turn
	// it on only for a receiving MX that prepends A-R to every message.
	TrustAuthResults bool
	// TrustedAuthServ are authserv-ids whose Authentication-Results header is
	// trusted when TrustAuthResults is on; empty = DefaultTrustedAuthServ.
	TrustedAuthServ []string
	// TrustARCSealers turns on ARC trust (OFF when empty): when direct DKIM
	// of a message fails, its TOP ARC set is accepted if sealed by one of
	// these d= domains, the seal verifies, the ARC-Authentication-Results'
	// authserv-id is in TrustARCAuthServ, and it records dkim=pass for a
	// domain aligned with the sender's. For a mailbox whose storage alters
	// bodies after its MX verified them (ForwardEmail). Both lists are
	// required together.
	TrustARCSealers  []string
	TrustARCAuthServ []string
	// TrustARCRecipients are this mailbox's own addresses as they appear in
	// To: an ARC-accepted message must have exactly one To naming one of
	// them. Required with ARC trust.
	TrustARCRecipients []string
	// ARCMaxAge bounds how long before receipt the top seal was made; 0 =
	// DefaultARCMaxAge.
	ARCMaxAge time.Duration
	// Forwarders are addresses whose DKIM-verified forwards of this sender's
	// mail are accepted as the sender's (the household's own mailboxes).
	Forwarders []string
	// DNSTimeout bounds one DKIM key lookup; 0 = DefaultDNSTimeout. A whole
	// message's verification gets three times that.
	DNSTimeout time.Duration
	// LookupTXT resolves DKIM public keys; nil = the system resolver.
	LookupTXT    func(domain string) ([]string, error)
	LookbackDays int
	// NewestFirst hands messages to the parser newest arrival first, so a
	// parser with a call budget (promo-vision-v1) spends it on current mail.
	NewestFirst bool
	MaxBytes    int64
	Parser      Parser
	Now         func() time.Time
	Logf        func(string, ...any)
}

// Connector implements listing.Connector.
type Connector struct {
	cfg          Config
	mu           sync.Mutex
	lastComplete bool
	// verdicts remembers, per UID key, a message this connector already
	// refused for a PERMANENT reason, so it is not fetched and re-verified
	// (a DNS lookup each) on every poll of the lookback window (rv35b
	// NEW-10). Bounded; entries expire so a sender that fixes its DKIM is
	// picked up again.
	verdicts map[string]verdict
}

type verdict struct {
	why string
	at  time.Time
}

const (
	maxVerdicts = 10000
	verdictTTL  = 24 * time.Hour
)

// remembered returns a fresh stored verdict for a UID key.
func (c *Connector) remembered(key string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.verdicts[key]
	if !ok || now.Sub(v.at) > verdictTTL {
		delete(c.verdicts, key)
		return "", false
	}
	return v.why, true
}

func (c *Connector) remember(key, why string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.verdicts == nil {
		c.verdicts = map[string]verdict{}
	}
	if len(c.verdicts) >= maxVerdicts {
		// Full: forget everything. The cost is one more verification each;
		// the bound is what matters.
		c.verdicts = map[string]verdict{}
	}
	c.verdicts[key] = verdict{why: why, at: now}
}

// NewConnector validates and fills defaults.
func NewConnector(cfg Config) (*Connector, error) {
	cfg.From = strings.ToLower(strings.TrimSpace(cfg.From))
	senders := make([]string, 0, len(cfg.Senders))
	for _, a := range cfg.Senders {
		a = strings.ToLower(strings.TrimSpace(a))
		if !strings.Contains(a, "@") {
			return nil, fmt.Errorf("imapmail: sender %q is not an address", a)
		}
		if a != cfg.From && !slices.Contains(senders, a) {
			senders = append(senders, a)
		}
	}
	cfg.Senders = senders
	if cfg.Name == "" || (cfg.From == "" && len(cfg.Senders) == 0) || (cfg.From != "" && !strings.Contains(cfg.From, "@")) {
		return nil, errors.New("imapmail: a source needs a name and a sender address (from, or a senders list)")
	}
	if cfg.Parser == nil {
		return nil, errors.New("imapmail: a source needs a parser")
	}
	if cfg.Host == "" || cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("imapmail: host, username and password are required")
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	if cfg.TLS == "" {
		cfg.TLS = "implicit"
	}
	if cfg.Port == "" {
		cfg.Port = "993"
		if cfg.TLS == "none" {
			cfg.Port = "143"
		}
	}
	if cfg.DKIMDomain == "" && cfg.From != "" {
		cfg.DKIMDomain = domainOf(cfg.From)
	}
	fwd := make([]string, 0, len(cfg.Forwarders))
	for _, f := range cfg.Forwarders {
		f = strings.ToLower(strings.TrimSpace(f))
		if !strings.Contains(f, "@") || f == cfg.From || slices.Contains(cfg.Senders, f) {
			return nil, fmt.Errorf("imapmail: forwarder %q must be an address other than the sender", f)
		}
		fwd = append(fwd, f)
	}
	cfg.Forwarders = fwd
	cfg.DKIMDomain = strings.ToLower(cfg.DKIMDomain)
	sealers, authserv := normDomains(cfg.TrustARCSealers), normDomains(cfg.TrustARCAuthServ)
	if (len(sealers) == 0) != (len(authserv) == 0) {
		return nil, errors.New("imapmail: ARC trust needs both trusted sealers and trusted authserv-ids")
	}
	for _, d := range append(append([]string{}, sealers...), authserv...) {
		if !validDomain(d) {
			return nil, fmt.Errorf("imapmail: ARC trust: %q is not a domain name", d)
		}
	}
	var recips []string
	for _, a := range cfg.TrustARCRecipients {
		a = strings.ToLower(strings.TrimSpace(a))
		if _, err := netmail.ParseAddress(a); err != nil || !strings.Contains(a, "@") {
			return nil, fmt.Errorf("imapmail: ARC trust: recipient %q is not an address", a)
		}
		if !slices.Contains(recips, a) {
			recips = append(recips, a)
		}
	}
	if len(sealers) > 0 && len(recips) == 0 {
		return nil, errors.New("imapmail: ARC trust needs the mailbox's recipient addresses (TrustARCRecipients)")
	}
	if cfg.ARCMaxAge < 0 {
		return nil, errors.New("imapmail: ARC max age must not be negative")
	}
	cfg.TrustARCSealers, cfg.TrustARCAuthServ, cfg.TrustARCRecipients = sealers, authserv, recips
	if cfg.TrustAuthResults && len(cfg.TrustedAuthServ) == 0 {
		cfg.TrustedAuthServ = DefaultTrustedAuthServ
	}
	if cfg.LookbackDays <= 0 {
		cfg.LookbackDays = DefaultLookbackDays
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Connector{cfg: cfg}, nil
}

// SourceID returns "imap:<Name>".
func (c *Connector) SourceID() string { return SourceID + ":" + c.cfg.Name }

// FetchComplete reports whether the last Fetch searched and read the mailbox.
func (c *Connector) FetchComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastComplete
}

// Fetch reads the sender's verified mail over the lookback window and returns
// the offers its parser finds.
func (c *Connector) Fetch(ctx context.Context) ([]listing.Raw, error) {
	c.setComplete(false)
	cl, err := c.dial()
	if err != nil {
		return nil, fmt.Errorf("imapmail %s: %w", c.cfg.Name, err)
	}
	defer func() { _ = cl.Close() }()
	if err := cl.Login(c.cfg.Username, c.cfg.Password).Wait(); err != nil {
		return nil, fmt.Errorf("imapmail %s: login: %w", c.cfg.Name, err)
	}
	// EXAMINE, not SELECT: the mailbox is never modified.
	sel, err := cl.Select(c.cfg.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, fmt.Errorf("imapmail %s: examine %q: %w", c.cfg.Name, c.cfg.Mailbox, err)
	}
	now := c.cfg.Now()
	since := now.AddDate(0, 0, -c.cfg.LookbackDays)
	// One search per address (the senders, then each forwarder), unioned.
	seen := map[imap.UID]bool{}
	var uids []imap.UID
	for _, addr := range c.addresses() {
		criteria := &imap.SearchCriteria{
			Since:  since,
			Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: addr}},
		}
		found, err := cl.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return nil, fmt.Errorf("imapmail %s: search %s: %w", c.cfg.Name, addr, err)
		}
		for _, u := range found.AllUIDs() {
			if !seen[u] {
				seen[u] = true
				uids = append(uids, u)
			}
		}
	}
	if c.cfg.CountIgnored {
		// Everything else in the window, by UID only: counted, never fetched.
		all, err := cl.UIDSearch(&imap.SearchCriteria{Since: since}, nil).Wait()
		if err != nil {
			return nil, fmt.Errorf("imapmail %s: search all: %w", c.cfg.Name, err)
		}
		for _, u := range all.AllUIDs() {
			if !seen[u] {
				c.observe(Observation{Key: fmt.Sprintf("uid:%d:%d", sel.UIDValidity, u), Reason: SkipUnknownSender})
			}
		}
	}
	var out []listing.Raw
	skipped := map[string]int{}
	uidKey := func(u imap.UID) string { return fmt.Sprintf("uid:%d:%d", sel.UIDValidity, u) }
	// Envelopes first: SEARCH FROM matched a substring (a display name can
	// hold an allowlisted address), so only mail whose parsed From is
	// exactly a searched address, and not too large, has its body fetched.
	received := map[imap.UID]time.Time{}
	var fetchUIDs []imap.UID
	if len(uids) > 0 {
		envs, err := cl.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, Envelope: true, RFC822Size: true, InternalDate: true}).Collect()
		if err != nil {
			return nil, fmt.Errorf("imapmail %s: fetch envelopes: %w", c.cfg.Name, err)
		}
		addrs := c.addresses()
		for _, m := range envs {
			received[m.UID] = m.InternalDate
			if m.Envelope == nil || len(m.Envelope.From) != 1 || !slices.Contains(addrs, strings.ToLower(m.Envelope.From[0].Addr())) {
				skipped[whyNotSender]++
				c.observe(Observation{Key: uidKey(m.UID), Reason: SkipUnknownSender, Received: m.InternalDate})
				continue
			}
			if m.RFC822Size > c.cfg.MaxBytes {
				skipped["too large"]++
				c.observe(Observation{Key: uidKey(m.UID), Reason: SkipTooLarge, Received: m.InternalDate})
				continue
			}
			if why, ok := c.remembered(uidKey(m.UID), now); ok {
				// Refused on an earlier poll: reported again, not re-read.
				skipped[why]++
				c.observe(Observation{Key: uidKey(m.UID), Reason: skipReasonFor(why), Received: m.InternalDate})
				continue
			}
			fetchUIDs = append(fetchUIDs, m.UID)
		}
	}
	if len(fetchUIDs) > 0 {
		set := imap.UIDSetNum(fetchUIDs...)
		opts := &imap.FetchOptions{UID: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}
		msgs, err := cl.Fetch(set, opts).Collect()
		if err != nil {
			return nil, fmt.Errorf("imapmail %s: fetch: %w", c.cfg.Name, err)
		}
		if c.cfg.NewestFirst {
			sort.SliceStable(msgs, func(i, j int) bool { return received[msgs[i].UID].After(received[msgs[j].UID]) })
		}
		for _, m := range msgs {
			at := received[m.UID]
			if m.RFC822Size > c.cfg.MaxBytes {
				skipped["too large"]++
				c.observe(Observation{Key: uidKey(m.UID), Reason: SkipTooLarge, Received: at})
				continue
			}
			var raw []byte
			for _, b := range m.BodySection {
				raw = b.Bytes
			}
			msg, why, temporary := c.verifyMessage(raw, at)
			if why != "" {
				skipped[why]++
				if !temporary {
					c.remember(uidKey(m.UID), why, now)
				}
				// Keyed by UID, never by the Message-ID: a spoofer writes
				// that, and reusing one would hide later spoofs (rv35b NEW-4).
				c.observe(Observation{Key: uidKey(m.UID), MessageID: msg.ID, Reason: skipReasonFor(why), Received: at})
				continue
			}
			msg.Received = at
			raws, err := c.cfg.Parser.Parse(msg)
			if err != nil {
				skipped["parse: "+err.Error()]++
				c.observe(Observation{Key: uidKey(m.UID), MessageID: msg.ID, Reason: SkipParseError, Received: at})
				continue
			}
			for i := range raws {
				raws[i].SourceID = c.SourceID()
				if raws[i].SourceKey == "" {
					raws[i].SourceKey = fmt.Sprintf("%s#%d", msg.ID, i)
				}
				if raws[i].Aspects == nil {
					raws[i].Aspects = map[string]string{}
				}
				raws[i].Aspects["mail_message_id"] = msg.ID
				if msg.ForwardedBy != "" {
					raws[i].Aspects["mail_forwarded_by"] = msg.ForwardedBy
					// A claim: the forwarded text's own From: line.
					raws[i].Aspects["mail_forwarded_from"] = msg.ForwardedFrom
				}
				raws[i].SeenAt = now
				// The deal stays listed while its mail is inside the
				// lookback window this connector reads: arrival (the
				// server's INTERNALDATE, which the sender cannot set)
				// plus that window. nagus's retention, not the seller's
				// end date -- surfaced with basis "email_window".
				if !at.IsZero() {
					raws[i].EndsAt = at.AddDate(0, 0, c.cfg.LookbackDays).UTC()
				}
			}
			out = append(out, raws...)
		}
	}
	if c.cfg.Logf != nil {
		c.cfg.Logf("imapmail %s: %d messages from %d sender(s) in %d days -> %d offers, skipped %v",
			c.cfg.Name, len(uids), len(c.senders()), c.cfg.LookbackDays, len(out), skipped)
	}
	c.setComplete(true)
	return out, nil
}

// senders is every address this source reads as a sender.
func (c *Connector) senders() []string {
	out := make([]string, 0, 1+len(c.cfg.Senders))
	if c.cfg.From != "" {
		out = append(out, c.cfg.From)
	}
	return append(out, c.cfg.Senders...)
}

// addresses is every address searched: the senders, then the forwarders.
func (c *Connector) addresses() []string {
	return append(c.senders(), c.cfg.Forwarders...)
}

// senderDomain is the domain a sender's DKIM signature must align to.
func (c *Connector) senderDomain(addr string) string {
	if addr == c.cfg.From && c.cfg.DKIMDomain != "" {
		return c.cfg.DKIMDomain
	}
	return domainOf(addr)
}

func (c *Connector) observe(o Observation) {
	if c.cfg.Observe != nil {
		c.cfg.Observe(o)
	}
}

// Skip texts returned by verify, mapped onto SkipReason by skipReasonFor.
const (
	whyNotSender   = "not from the declared sender"
	whyNoDKIM      = "no verified dkim pass"
	whyNoID        = "no message-id"
	whyOtherSender = "forward of another sender"
	whyDupHeader   = "duplicate identity header"
)

// singleHeaders may occur at most once (From exactly once): a duplicate is
// how a second identity is smuggled past a signature over the first.
var singleHeaders = []string{"From", "Sender", "Subject", "Message-Id"}

func skipReasonFor(why string) SkipReason {
	switch why {
	case whyNotSender:
		return SkipUnknownSender
	case whyNoDKIM, whyDupHeader:
		// A duplicated identity header reached verify only because its
		// envelope From is an allowlisted address: it is an attempt on that
		// identity, and counts (and alerts) with the unverified.
		return SkipUnverified
	default:
		return SkipInvalid
	}
}

func (c *Connector) dial() (*imapclient.Client, error) {
	addr := net.JoinHostPort(c.cfg.Host, c.cfg.Port)
	opts := &imapclient.Options{TLSConfig: &tls.Config{ServerName: c.cfg.Host, MinVersion: tls.VersionTLS12}}
	switch c.cfg.TLS {
	case "implicit":
		return imapclient.DialTLS(addr, opts)
	case "starttls":
		return imapclient.DialStartTLS(addr, opts)
	case "none":
		return imapclient.DialInsecure(addr, nil)
	default:
		return nil, fmt.Errorf("tls mode %q (want implicit, starttls or none)", c.cfg.TLS)
	}
}

// verify parses a message and accepts it only from the declared sender (or a
// declared forwarder) with a DKIM pass for that address's domain. A non-empty
// reason means it was skipped.
func (c *Connector) verify(raw []byte) (Message, string) {
	m, why, _ := c.verifyMessage(raw, time.Time{})
	return m, why
}

// verifyAt is verify for a message the server received at received (IMAP
// INTERNALDATE), which the ARC path needs for its recency check.
func (c *Connector) verifyAt(raw []byte, received time.Time) (Message, string) {
	m, why, _ := c.verifyMessage(raw, received)
	return m, why
}

// verifyMessage is verify, also reporting whether a refusal is TEMPORARY (a
// DNS lookup that failed or timed out): only permanent refusals are
// remembered.
func (c *Connector) verifyMessage(raw []byte, received time.Time) (Message, string, bool) {
	// Atomic: go-msgauth verifies a message's signatures concurrently, so
	// several key lookups may report a temporary failure at once (rv35c R1).
	var temporary atomic.Bool
	m, why := c.verifyWith(raw, received, &temporary)
	return m, why, temporary.Load() && why == whyNoDKIM
}

func (c *Connector) verifyWith(raw []byte, received time.Time, temporary *atomic.Bool) (Message, string) {
	mr, err := mail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		return Message{}, "unparseable"
	}
	h := mr.Header
	for _, k := range singleHeaders {
		if len(h.Values(k)) > 1 {
			return Message{}, whyDupHeader
		}
	}
	if len(h.Values("From")) != 1 {
		return Message{}, whyNotSender
	}
	id, _ := h.MessageID()
	from, err := h.AddressList("From")
	if err != nil || len(from) != 1 {
		return Message{}, whyNotSender
	}
	addr := strings.ToLower(from[0].Address)
	forwarder := ""
	var domain string
	if slices.Contains(c.senders(), addr) {
		domain = c.senderDomain(addr)
	} else {
		for _, f := range c.cfg.Forwarders {
			if addr == f {
				forwarder, domain = f, domainOf(f)
			}
		}
		if forwarder == "" {
			return Message{}, whyNotSender
		}
	}
	path := ""
	switch {
	case c.cfg.TrustAuthResults && c.dkimVerified(h.Values("Authentication-Results"), domain):
		path = PathAuthResults
	case c.dkimSigned(raw, domain, temporary):
		path = PathDKIM
	case len(c.cfg.TrustARCSealers) > 0 && c.arcVerified(raw, h, addr, domain, received, temporary):
		// Only after direct DKIM failed, and only for a source that opted
		// in: the mailbox's own MX verified DKIM on receipt and sealed
		// that verdict; its storage later altered the body.
		path = PathARC
	}
	if path != "" {
		countAuth(path)
	}
	if path == "" {
		if c.cfg.Logf != nil {
			c.cfg.Logf("imapmail %s: REJECTED a message claiming to be from %s without a verified DKIM pass for %s (possible spoof)",
				c.cfg.Name, addr, domain)
		}
		// The id is returned (unverified) only so the skip can be counted
		// once per message; the message itself is not.
		return Message{ID: id}, whyNoDKIM
	}
	if id == "" {
		return Message{}, whyNoID
	}
	subject, _ := h.Subject()
	date, _ := h.Date()
	msg := Message{ID: id, From: addr, Subject: subject, Date: date, ForwardedBy: forwarder,
		Reply: h.Has("In-Reply-To") || h.Has("References")}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Message{}, "unparseable body"
		}
		ih, ok := p.Header.(*mail.InlineHeader)
		if !ok {
			continue // attachments are never read
		}
		ct, params, _ := ih.ContentType()
		b, err := io.ReadAll(io.LimitReader(p.Body, c.cfg.MaxBytes))
		if err != nil {
			return Message{}, "unreadable part"
		}
		switch ct {
		case "text/plain":
			if msg.Text == "" {
				msg.Text = string(b)
				if strings.EqualFold(params["format"], "flowed") {
					msg.Text = unflow(msg.Text, strings.EqualFold(params["delsp"], "yes"))
				}
			}
		case "text/html":
			if msg.HTML == "" {
				msg.HTML = string(b)
			}
		}
	}
	if forwarder != "" {
		orig := forwardedFrom(msg.Text)
		if orig == "" {
			orig = forwardedFrom(htmlText(msg.HTML))
		}
		if orig == "" || !slices.Contains(c.senders(), orig) {
			return Message{}, whyOtherSender
		}
		// Credit stays with the forwarder: orig is only what the forwarded
		// text says (rv35 N1).
		msg.ForwardedFrom = orig
	}
	return msg, ""
}

// unflow undoes RFC 3676 format=flowed: a line ending in a space is joined
// to the next (the space removed too with delsp=yes). Space-stuffing is
// removed; the signature separator "-- " is never joined.
func unflow(text string, delsp bool) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var b strings.Builder
	for i, ln := range lines {
		ln = strings.TrimPrefix(ln, " ") // space-stuffed
		soft := strings.HasSuffix(ln, " ") && ln != "-- " && i < len(lines)-1
		if soft && delsp {
			ln = ln[:len(ln)-1]
		}
		b.WriteString(ln)
		if !soft && i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// forwardMarker opens the quoted original in a forward: Gmail's
// "---------- Forwarded message ---------" and Apple Mail's
// "Begin forwarded message:".
var forwardMarker = regexp.MustCompile(`(?im)^[\s>]*(?:-{3,}\s*forwarded message\s*-{3,}|begin forwarded message:)\s*$`)

// forwardedFrom returns the lower-cased address on the first From: line of
// the first forwarded original in text, or "".
func forwardedFrom(text string) string {
	loc := forwardMarker.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	lines := strings.Split(text[loc[1]:], "\n")
	for i, ln := range lines {
		if i > 12 {
			break
		}
		ln = strings.TrimLeft(strings.TrimSpace(ln), "> ")
		v, ok := strings.CutPrefix(ln, "From:")
		if !ok {
			continue
		}
		if a, err := netmail.ParseAddress(strings.TrimSpace(v)); err == nil {
			return strings.ToLower(a.Address)
		}
		return ""
	}
	return ""
}

var (
	htmlBreak = regexp.MustCompile(`(?i)<br\s*/?>|</(?:div|p|tr|li)>`)
	htmlTag   = regexp.MustCompile(`<[^>]*>`)
)

// htmlText is a rough text rendering, enough to find a forward header in an
// HTML-only forward.
func htmlText(s string) string {
	s = htmlBreak.ReplaceAllString(s, "\n")
	return html.UnescapeString(htmlTag.ReplaceAllString(s, ""))
}

// dkimSigned verifies the message's DKIM signatures itself and reports whether
// one that verifies is aligned to domain. Bare LF line endings are restored to
// CRLF first: DKIM canonicalization is defined over CRLF.
//
// Key lookups are bounded: DNSTimeout each, and three times that for the
// whole message (rv35b NEW-10). *temporary is set when a lookup failed
// temporarily or timed out, so the caller does not remember the refusal.
func (c *Connector) dkimSigned(raw []byte, domain string, temporary *atomic.Bool) bool {
	norm := bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(norm), &dkim.VerifyOptions{LookupTXT: c.boundedLookup(temporary), MaxVerifications: 5})
	if err != nil {
		return false
	}
	for _, v := range verifs {
		if v.Err == nil && aligned(strings.ToLower(v.Domain), domain) && coversFrom(v.HeaderKeys) {
			return true
		}
	}
	return false
}

// Authentication paths, the closed label set of AuthStats.
const (
	PathDKIM        = "dkim"
	PathAuthResults = "auth_results"
	PathARC         = "arc"
)

// AuthPaths in metric order.
var AuthPaths = []string{PathDKIM, PathAuthResults, PathARC}

var authCounts sync.Map // path -> *atomic.Int64

func countAuth(path string) {
	v, _ := authCounts.LoadOrStore(path, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
}

// AuthStats returns, per path, how many message verifications that path
// authenticated in this process (every poll re-verifies the window, so these
// count verifications, not distinct messages).
func AuthStats() map[string]int64 {
	out := map[string]int64{}
	for _, p := range AuthPaths {
		if v, ok := authCounts.Load(p); ok {
			out[p] = v.(*atomic.Int64).Load()
		} else {
			out[p] = 0
		}
	}
	return out
}

// DefaultARCMaxAge is how long before the server received a message its
// top ARC seal may have been made.
const DefaultARCMaxAge = time.Hour

// arcClockSkew is how far after the receive time a seal may claim to be.
const arcClockSkew = 5 * time.Minute

// arcVerified reports whether the message's top ARC set makes it genuine
// mail from domain, delivered to THIS mailbox recently (nagus-25g; see
// internal/arc for what the seal and the AMS bind):
//
//   - the top seal is by a trusted sealer and verifies, and the top AMS
//     verifies over this message's From, To, Subject and Date (the body is
//     not bound: storage rewrote it);
//   - the seal was made within ARCMaxAge before the server received the
//     message (INTERNALDATE), so an old seal cannot be replayed;
//   - the message has exactly one To, naming one of TrustARCRecipients: a
//     seal replayed from another mailbox carries that mailbox's address;
//   - the top ARC-Authentication-Results comes from a trusted authserv-id and
//     records dkim=pass for a domain aligned with domain (the same rule as
//     direct DKIM); when it names the signature (header.b, header.s), a
//     DKIM-Signature of this message must match it.
func (c *Connector) arcVerified(raw []byte, h mail.Header, addr, domain string, received time.Time, temporary *atomic.Bool) bool {
	if received.IsZero() {
		return false
	}
	ch, err := arc.Verify(raw, arc.Options{LookupTXT: c.boundedLookup(temporary), TrustedSealers: c.cfg.TrustARCSealers})
	if err != nil {
		return false
	}
	maxAge := c.cfg.ARCMaxAge
	if maxAge <= 0 {
		maxAge = DefaultARCMaxAge
	}
	if ch.SealTime.After(received.Add(arcClockSkew)) || ch.SealTime.Before(received.Add(-maxAge)) {
		return false
	}
	if len(h.Values("To")) != 1 {
		return false
	}
	// EXACTLY one address, and it is this mailbox's: with several, a
	// co-recipient of a genuine sealed message could replay it with another
	// body (the body is not bound). A group with one member reduces to that
	// member; an empty group has none. Bcc co-recipients are invisible here
	// and are the accepted residual risk (design doc, "ARC trust").
	to, err := h.AddressList("To")
	if err != nil || len(to) != 1 || !slices.Contains(c.cfg.TrustARCRecipients, strings.ToLower(to[0].Address)) {
		return false
	}
	if !slices.Contains(c.cfg.TrustARCAuthServ, ch.AuthServID) {
		return false
	}
	sigs := dkimSignatures(h)
	for _, r := range ch.Results {
		if r.Method != "dkim" || r.Value != "pass" {
			continue
		}
		d := strings.ToLower(r.Props["header.d"])
		if d == "" {
			if i := r.Props["header.i"]; strings.Contains(i, "@") {
				d = strings.ToLower(i[strings.LastIndex(i, "@")+1:])
			}
		}
		if !aligned(d, domain) || !namesASignature(r.Props, d, sigs) {
			continue
		}
		if c.cfg.Logf != nil {
			c.cfg.Logf("imapmail %s: accepted a message from %s via the ARC seal of %s (i=%d, authserv %s, dkim=pass for %s); direct DKIM of the stored copy failed",
				c.cfg.Name, addr, ch.Sealer, ch.Instance, ch.AuthServID, d)
		}
		return true
	}
	return false
}

type dkimSig struct{ d, s, b string }

// dkimSignatures are the message's DKIM-Signature d=, s= and b= values.
func dkimSignatures(h mail.Header) []dkimSig {
	var out []dkimSig
	for _, v := range h.Values("DKIM-Signature") {
		sig := dkimSig{}
		for _, part := range strings.Split(v, ";") {
			k, val, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			val = strings.Join(strings.Fields(val), "")
			switch strings.TrimSpace(k) {
			case "d":
				sig.d = strings.ToLower(val)
			case "s":
				sig.s = val
			case "b":
				sig.b = val
			}
		}
		out = append(out, sig)
	}
	return out
}

// minHeaderB is the shortest header.b prefix accepted (RFC 6008 asks for
// at least 8 characters).
const minHeaderB = 8

// namesASignature: when the result names a signature (header.b, a prefix of
// its b= of at least minHeaderB characters; header.s, its selector), one of
// the message's DKIM-Signatures by domain d must match it. A result that
// names none matches. This is a CONSISTENCY check, not a security boundary:
// the verdict's integrity comes from the seal, and the quote-aware AAR
// tokenizer is what stops a smuggled identity.
func namesASignature(props map[string]string, d string, sigs []dkimSig) bool {
	b, s := props["header.b"], props["header.s"]
	if b == "" && s == "" {
		return true
	}
	if b != "" && len(b) < minHeaderB {
		return false
	}
	for _, sig := range sigs {
		if aligned(sig.d, d) && aligned(d, sig.d) && (b == "" || strings.HasPrefix(sig.b, b)) && (s == "" || sig.s == s) {
			return true
		}
	}
	return false
}

// coversFrom reports whether a signature's h= covers From. go-msgauth
// already refuses one that does not; this keeps the rule explicit here.
func coversFrom(keys []string) bool {
	for _, k := range keys {
		if strings.EqualFold(strings.TrimSpace(k), "from") {
			return true
		}
	}
	return false
}

// boundedLookup wraps the DKIM key lookup with a per-lookup timeout and a
// per-message budget. A lookup that does not answer in time is a TEMPORARY
// failure.
func (c *Connector) boundedLookup(temporary *atomic.Bool) func(string) ([]string, error) {
	per := c.cfg.DNSTimeout
	if per <= 0 {
		per = DefaultDNSTimeout
	}
	deadline := time.Now().Add(3 * per)
	base := c.cfg.LookupTXT
	return func(name string) ([]string, error) {
		wait := min(per, time.Until(deadline))
		timeout := &net.DNSError{Err: "dkim key lookup timed out", Name: name, IsTimeout: true, IsTemporary: true}
		if wait <= 0 {
			temporary.Store(true)
			return nil, timeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		type answer struct {
			txt []string
			err error
		}
		ch := make(chan answer, 1)
		go func() {
			var a answer
			if base != nil {
				a.txt, a.err = base(name)
			} else {
				a.txt, a.err = net.DefaultResolver.LookupTXT(ctx, name)
			}
			ch <- a
		}()
		select {
		case a := <-ch:
			var ne net.Error
			if errors.As(a.err, &ne) && (ne.Timeout() || isTemporary(a.err)) {
				temporary.Store(true)
			}
			return a.txt, a.err
		case <-ctx.Done():
			temporary.Store(true)
			return nil, timeout
		}
	}
}

func isTemporary(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsTemporary
}

// aligned is relaxed DKIM alignment with a public-suffix floor: the signing
// domain d is the From domain itself or one of its parents, and no shallower
// than the From domain's registrable domain (eTLD+1). So d=example.org
// vouches for mail.example.org, but d=org or d=co.uk vouches for nothing
// (rv35b). When the registrable domain cannot be determined, d must have at
// least two labels.
func aligned(d, domain string) bool {
	if d == "" || (d != domain && !strings.HasSuffix(domain, "."+d)) {
		return false
	}
	if reg, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		return d == reg || strings.HasSuffix(d, "."+reg)
	}
	return strings.Contains(d, ".")
}

func normDomains(in []string) []string {
	var out []string
	for _, d := range in {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

var domainRe = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// validDomain is a lower-case DNS name of at least two labels.
func validDomain(d string) bool { return len(d) <= 253 && domainRe.MatchString(d) }

func domainOf(addr string) string {
	return strings.ToLower(addr[strings.LastIndex(addr, "@")+1:])
}

// dkimVerified reads ONLY the topmost Authentication-Results header: headers
// are prepended in transit, so the first is the one our receiving MX wrote.
// Any deeper one could have been written by the sender and proves nothing.
func (c *Connector) dkimVerified(ars []string, domain string) bool {
	if len(ars) == 0 {
		return false
	}
	top := ars[0]
	servID := strings.ToLower(strings.TrimSpace(strings.SplitN(top, ";", 2)[0]))
	if f := strings.Fields(servID); len(f) > 0 {
		servID = f[0]
	}
	trusted := false
	for _, t := range c.cfg.TrustedAuthServ {
		t = strings.ToLower(t)
		if servID == t || strings.HasSuffix(servID, "."+t) {
			trusted = true
		}
	}
	if !trusted {
		return false
	}
	for _, res := range strings.Split(top, ";")[1:] {
		f := strings.Fields(strings.ToLower(res))
		if len(f) == 0 || f[0] != "dkim=pass" {
			continue
		}
		for _, kv := range f[1:] {
			if d, ok := strings.CutPrefix(kv, "header.d="); ok {
				if aligned(strings.Trim(d, `"`), domain) {
					return true
				}
			}
		}
	}
	return false
}

func (c *Connector) setComplete(v bool) {
	c.mu.Lock()
	c.lastComplete = v
	c.mu.Unlock()
}
