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
//   - A From header is trivially forged. A message is accepted only when the
//     TOPMOST Authentication-Results header -- the one our receiving MX
//     prepended on arrival -- comes from a trusted authserv-id and reports
//     dkim=pass for the sender's domain (or its parent, relaxed alignment).
//     Deeper A-R headers are attacker-writable and are never consulted.
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
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	_ "github.com/emersion/go-message/charset" // non-UTF-8 bodies
	"github.com/emersion/go-message/mail"

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
	ID      string // Message-ID, without angle brackets
	From    string // the verified sender address, lower-cased
	Subject string
	Date    time.Time
	Text    string // the text/plain part, if any
	HTML    string // the text/html part, if any
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
	LookbackDays    int
	MaxBytes        int64
	Parser          Parser
	Now             func() time.Time
	Logf            func(string, ...any)
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
		cfg.DKIMDomain = cfg.From[strings.LastIndex(cfg.From, "@")+1:]
	}
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
	criteria := &imap.SearchCriteria{
		Since:  now.AddDate(0, 0, -c.cfg.LookbackDays),
		Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: c.cfg.From}},
	}
	found, err := cl.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imapmail %s: search: %w", c.cfg.Name, err)
	}
	uids := found.AllUIDs()
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

// verify parses a message and accepts it only from the declared sender with
// a DKIM pass our MX recorded. A non-empty reason means it was skipped.
func (c *Connector) verify(raw []byte) (Message, string) {
	mr, err := mail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		return Message{}, "unparseable"
	}
	h := mr.Header
	from, err := h.AddressList("From")
	if err != nil || len(from) != 1 || strings.ToLower(from[0].Address) != c.cfg.From {
		return Message{}, "not from the declared sender"
	}
	if !c.dkimVerified(h.Values("Authentication-Results")) {
		if c.cfg.Logf != nil {
			c.cfg.Logf("imapmail %s: REJECTED a message claiming to be from %s without a verified DKIM pass for %s (possible spoof)",
				c.cfg.Name, c.cfg.From, c.cfg.DKIMDomain)
		}
		return Message{}, "no verified dkim pass"
	}
	id, _ := h.MessageID()
	if id == "" {
		return Message{}, "no message-id"
	}
	subject, _ := h.Subject()
	date, _ := h.Date()
	msg := Message{ID: id, From: c.cfg.From, Subject: subject, Date: date}
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
	return msg, ""
}

// dkimVerified reads ONLY the topmost Authentication-Results header: headers
// are prepended in transit, so the first is the one our receiving MX wrote.
// Any deeper one could have been written by the sender and proves nothing.
func (c *Connector) dkimVerified(ars []string) bool {
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
				d = strings.Trim(d, `"`)
				if d == c.cfg.DKIMDomain || strings.HasSuffix(c.cfg.DKIMDomain, "."+d) {
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
