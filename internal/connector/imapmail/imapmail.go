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
//     2. The TOPMOST Authentication-Results header -- the one a receiving
//     MX prepends on arrival -- comes from a trusted authserv-id and
//     reports dkim=pass. Deeper A-R headers are attacker-writable and are
//     never consulted. Kept for a provider that does write A-R.
//     ARC sets are deliberately NOT trusted without verifying the seal chain:
//     lower instances are sender-writable.
//   - Forwarded mail (nagus-zsi, option 3). A source may name Forwarders --
//     the household's own mailboxes, from config only. A message From a
//     forwarder, with a DKIM pass for the FORWARDER's domain, whose forwarded
//     original names this source's sender, is that sender's mail: a person
//     forwarding a newsletter by hand vouches for it. It is credited to the
//     sender, marked mail_forwarded_by.
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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	_ "github.com/emersion/go-message/charset" // non-UTF-8 bodies
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-msgauth/dkim"

	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the connector family; a configured source is "imap:<Name>".
const SourceID = "imap"

// DefaultLookbackDays is the window searched each poll.
const DefaultLookbackDays = 14

// DefaultMaxBytes bounds one message; a larger one is skipped, not truncated.
const DefaultMaxBytes = 4 << 20

// DefaultTrustedAuthServ is the receiving MX whose Authentication-Results
// header is trusted: ForwardEmail holds the MX for apocryph.al.
var DefaultTrustedAuthServ = []string{"forwardemail.net"}

// Message is one verified email, handed to the sender's Parser.
type Message struct {
	ID   string // Message-ID, without angle brackets
	From string // the verified sender address, lower-cased
	// ForwardedBy is the forwarder address when this is a hand-forwarded copy
	// of the sender's mail; empty for mail the sender sent directly.
	ForwardedBy string
	Subject     string
	Date        time.Time
	Text        string // the text/plain part, if any
	HTML        string // the text/html part, if any
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
	// From is the one sender this source reads (required).
	From string
	// DKIMDomain is the domain the sender's DKIM signature must be aligned
	// to; empty = the domain of From.
	DKIMDomain string
	// TrustedAuthServ are authserv-ids whose Authentication-Results header is
	// trusted; empty = DefaultTrustedAuthServ.
	TrustedAuthServ []string
	// Forwarders are addresses whose DKIM-verified forwards of this sender's
	// mail are accepted as the sender's (the household's own mailboxes).
	Forwarders []string
	// LookupTXT resolves DKIM public keys; nil = the system resolver.
	LookupTXT    func(domain string) ([]string, error)
	LookbackDays int
	MaxBytes     int64
	Parser       Parser
	Now          func() time.Time
	Logf         func(string, ...any)
}

// Connector implements listing.Connector.
type Connector struct {
	cfg          Config
	mu           sync.Mutex
	lastComplete bool
}

// NewConnector validates and fills defaults.
func NewConnector(cfg Config) (*Connector, error) {
	cfg.From = strings.ToLower(strings.TrimSpace(cfg.From))
	if cfg.Name == "" || cfg.From == "" || !strings.Contains(cfg.From, "@") {
		return nil, errors.New("imapmail: a source needs a name and one sender address (from)")
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
	if cfg.DKIMDomain == "" {
		cfg.DKIMDomain = domainOf(cfg.From)
	}
	fwd := make([]string, 0, len(cfg.Forwarders))
	for _, f := range cfg.Forwarders {
		f = strings.ToLower(strings.TrimSpace(f))
		if !strings.Contains(f, "@") || f == cfg.From {
			return nil, fmt.Errorf("imapmail: forwarder %q must be an address other than the sender", f)
		}
		fwd = append(fwd, f)
	}
	cfg.Forwarders = fwd
	cfg.DKIMDomain = strings.ToLower(cfg.DKIMDomain)
	if len(cfg.TrustedAuthServ) == 0 {
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
	if _, err := cl.Select(c.cfg.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return nil, fmt.Errorf("imapmail %s: examine %q: %w", c.cfg.Name, c.cfg.Mailbox, err)
	}
	now := c.cfg.Now()
	// One search per address (the sender, then each forwarder), unioned.
	seen := map[imap.UID]bool{}
	var uids []imap.UID
	for _, addr := range append([]string{c.cfg.From}, c.cfg.Forwarders...) {
		criteria := &imap.SearchCriteria{
			Since:  now.AddDate(0, 0, -c.cfg.LookbackDays),
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
	var out []listing.Raw
	skipped := map[string]int{}
	if len(uids) > 0 {
		set := imap.UIDSetNum(uids...)
		opts := &imap.FetchOptions{RFC822Size: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}
		msgs, err := cl.Fetch(set, opts).Collect()
		if err != nil {
			return nil, fmt.Errorf("imapmail %s: fetch: %w", c.cfg.Name, err)
		}
		for _, m := range msgs {
			if m.RFC822Size > c.cfg.MaxBytes {
				skipped["too large"]++
				continue
			}
			var raw []byte
			for _, b := range m.BodySection {
				raw = b.Bytes
			}
			msg, why := c.verify(raw)
			if why != "" {
				skipped[why]++
				continue
			}
			raws, err := c.cfg.Parser.Parse(msg)
			if err != nil {
				skipped["parse: "+err.Error()]++
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
				}
				raws[i].SeenAt = now
			}
			out = append(out, raws...)
		}
	}
	if c.cfg.Logf != nil {
		c.cfg.Logf("imapmail %s: %d messages from %s in %d days -> %d offers, skipped %v",
			c.cfg.Name, len(uids), c.cfg.From, c.cfg.LookbackDays, len(out), skipped)
	}
	c.setComplete(true)
	return out, nil
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
	mr, err := mail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		return Message{}, "unparseable"
	}
	h := mr.Header
	from, err := h.AddressList("From")
	if err != nil || len(from) != 1 {
		return Message{}, "not from the declared sender"
	}
	addr := strings.ToLower(from[0].Address)
	forwarder := ""
	domain := c.cfg.DKIMDomain
	if addr != c.cfg.From {
		for _, f := range c.cfg.Forwarders {
			if addr == f {
				forwarder, domain = f, domainOf(f)
			}
		}
		if forwarder == "" {
			return Message{}, "not from the declared sender"
		}
	}
	if !c.dkimVerified(h.Values("Authentication-Results"), domain) && !c.dkimSigned(raw, domain) {
		if c.cfg.Logf != nil {
			c.cfg.Logf("imapmail %s: REJECTED a message claiming to be from %s without a verified DKIM pass for %s (possible spoof)",
				c.cfg.Name, addr, domain)
		}
		return Message{}, "no verified dkim pass"
	}
	id, _ := h.MessageID()
	if id == "" {
		return Message{}, "no message-id"
	}
	subject, _ := h.Subject()
	date, _ := h.Date()
	msg := Message{ID: id, From: c.cfg.From, Subject: subject, Date: date, ForwardedBy: forwarder}
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
		ct, _, _ := ih.ContentType()
		b, err := io.ReadAll(io.LimitReader(p.Body, c.cfg.MaxBytes))
		if err != nil {
			return Message{}, "unreadable part"
		}
		switch ct {
		case "text/plain":
			if msg.Text == "" {
				msg.Text = string(b)
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
		if orig != c.cfg.From {
			return Message{}, "forward of another sender"
		}
	}
	return msg, ""
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
func (c *Connector) dkimSigned(raw []byte, domain string) bool {
	norm := bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(norm), &dkim.VerifyOptions{LookupTXT: c.cfg.LookupTXT, MaxVerifications: 5})
	if err != nil {
		return false
	}
	for _, v := range verifs {
		if v.Err == nil && aligned(strings.ToLower(v.Domain), domain) {
			return true
		}
	}
	return false
}

// aligned is relaxed DKIM alignment: the signing domain d is the domain
// itself or one of its parents.
func aligned(d, domain string) bool {
	return d != "" && (d == domain || strings.HasSuffix(domain, "."+d))
}

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
