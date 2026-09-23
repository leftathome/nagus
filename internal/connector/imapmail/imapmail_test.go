package imapmail

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/leftathome/nagus/internal/listing"
)

var now = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// server runs a real IMAP server in-process and appends the given messages.
func server(t *testing.T, msgs map[string]time.Time) string {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("deals@totally.apocryph.al", "pw")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Login("deals@totally.apocryph.al", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	for raw, at := range msgs {
		cmd := c.Append("INBOX", int64(len(raw)), &imap.AppendOptions{Time: at})
		if _, err := cmd.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	return ln.Addr().String()
}

// eml builds a message. ar is the TOPMOST Authentication-Results header
// ("" = none); extra headers follow it.
func eml(id, from, subject, ar string, extra ...string) string {
	var b strings.Builder
	if ar != "" {
		b.WriteString("Authentication-Results: " + ar + "\r\n")
	}
	for _, h := range extra {
		b.WriteString(h + "\r\n")
	}
	fmt.Fprintf(&b, "Message-ID: <%s>\r\nFrom: Offers <%s>\r\nTo: deals@totally.apocryph.al\r\nSubject: %s\r\n", id, from, subject)
	b.WriteString("Date: Tue, 22 Sep 2026 08:00:00 +0000\r\nMIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString("Today only: 2019 Test Cabernet $29 (reg $59)\r\n")
	return b.String()
}

const sender = "offers@lastbottlewines.com"

func passAR(d string) string {
	return "mx1.forwardemail.net; dkim=pass header.d=" + d + " header.s=s1; spf=pass smtp.mailfrom=" + d
}

// recorder is a test parser: one offer per message, remembering what it saw.
type recorder struct{ seen []Message }

func (r *recorder) Parse(m Message) ([]listing.Raw, error) {
	r.seen = append(r.seen, m)
	return []listing.Raw{{Title: m.Subject, Body: m.Text, PriceCents: 2900, Currency: "USD"}}, nil
}

func connector(t *testing.T, addr string, p Parser) *Connector {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	c, err := NewConnector(Config{Name: "lastbottle", Host: host, Port: port, TLS: "none",
		Username: "deals@totally.apocryph.al", Password: "pw", From: sender, Parser: p,
		Now: func() time.Time { return now }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReadsOnlyTheDeclaredSendersVerifiedMail(t *testing.T) {
	recent := now.Add(-24 * time.Hour)
	addr := server(t, map[string]time.Time{
		eml("ok-1@lb", sender, "Todays offer", passAR("lastbottlewines.com")): recent,
		// a subdomain signer passes relaxed alignment
		eml("ok-2@lb", "offers@mail.lastbottlewines.com", "Subdomain", passAR("lastbottlewines.com")): recent,
		// another sender entirely: never read
		eml("other@x", "news@somewhere.com", "Not ours", passAR("somewhere.com")): recent,
		// a SPOOF: claims the sender, no DKIM pass
		eml("spoof-1@evil", sender, "Spoofed deal", "mx1.forwardemail.net; dkim=fail header.d=lastbottlewines.com"): recent,
		// a SPOOF with a forged A-R deeper in the headers; the top one is ours
		eml("spoof-2@evil", sender, "Forged AR", "mx1.forwardemail.net; dkim=none",
			"Authentication-Results: mx1.forwardemail.net; dkim=pass header.d=lastbottlewines.com"): recent,
		// a SPOOF whose only A-R was written by an untrusted host
		eml("spoof-3@evil", sender, "Untrusted AR", "evil.example; dkim=pass header.d=lastbottlewines.com"): recent,
		// DKIM for a DIFFERENT domain does not vouch for this sender
		eml("spoof-4@evil", sender, "Wrong d=", passAR("evil.example")): recent,
		// outside the lookback window
		eml("old@lb", sender, "Old offer", passAR("lastbottlewines.com")): now.AddDate(0, 0, -60),
	})
	rec := &recorder{}
	raws, err := connector(t, addr, rec).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, m := range rec.seen {
		subjects = append(subjects, m.Subject)
	}
	if len(rec.seen) != 1 || rec.seen[0].Subject != "Todays offer" {
		t.Fatalf("parser saw %v, want only the verified message from the declared sender", subjects)
	}
	if len(raws) != 1 {
		t.Fatalf("offers %d", len(raws))
	}
	r := raws[0]
	if r.SourceID != "imap:lastbottle" || r.SourceKey != "ok-1@lb#0" || r.Aspects["mail_message_id"] != "ok-1@lb" || !r.SeenAt.Equal(now) {
		t.Fatalf("raw %+v", r)
	}
	if !strings.Contains(rec.seen[0].Text, "2019 Test Cabernet $29") {
		t.Fatalf("text part not read: %q", rec.seen[0].Text)
	}
}

// The subdomain case must be declared: From is matched exactly, so a sender
// that mails from a subdomain is its own source.
func TestSubdomainSenderWithRelaxedAlignment(t *testing.T) {
	addr := server(t, map[string]time.Time{
		eml("ok-2@lb", "offers@mail.lastbottlewines.com", "Subdomain", passAR("lastbottlewines.com")): now.Add(-time.Hour),
	})
	host, port, _ := net.SplitHostPort(addr)
	rec := &recorder{}
	c, err := NewConnector(Config{Name: "lb-mail", Host: host, Port: port, TLS: "none",
		Username: "deals@totally.apocryph.al", Password: "pw", From: "offers@mail.lastbottlewines.com",
		Parser: rec, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.seen) != 1 {
		t.Fatalf("a parent-domain DKIM signature must pass relaxed alignment: saw %d", len(rec.seen))
	}
}

func TestDKIMVerification(t *testing.T) {
	c, err := NewConnector(Config{Name: "x", Host: "h", Username: "u", Password: "p", From: sender, Parser: &recorder{}})
	if err != nil {
		t.Fatal(err)
	}
	for ar, want := range map[string]bool{
		"mx1.forwardemail.net; dkim=pass header.d=lastbottlewines.com":               true,
		"mx2.forwardemail.net; spf=pass; dkim=pass header.d=\"lastbottlewines.com\"": true,
		"forwardemail.net; dkim=pass header.d=lastbottlewines.com":                   true,
		"mx1.forwardemail.net; dkim=fail header.d=lastbottlewines.com":               false,
		"mx1.forwardemail.net; dkim=pass header.d=notlastbottlewines.com":            false,
		"forwardemail.net.evil.example; dkim=pass header.d=lastbottlewines.com":      false,
		"evilforwardemail.net; dkim=pass header.d=lastbottlewines.com":               false,
	} {
		if got := c.dkimVerified([]string{ar}); got != want {
			t.Errorf("dkimVerified(%q) = %v, want %v", ar, got, want)
		}
	}
	if c.dkimVerified(nil) {
		t.Error("no Authentication-Results must not verify")
	}
}

func TestConfigValidation(t *testing.T) {
	base := Config{Name: "x", Host: "h", Username: "u", Password: "p", From: sender, Parser: &recorder{}}
	if _, err := NewConnector(base); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no from":   func(c *Config) { c.From = "" },
		"bad from":  func(c *Config) { c.From = "not-an-address" },
		"no parser": func(c *Config) { c.Parser = nil },
		"no host":   func(c *Config) { c.Host = "" },
	} {
		c := base
		mut(&c)
		if _, err := NewConnector(c); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := Lookup("nobody"); err == nil || !strings.Contains(err.Error(), "real captured email") {
		t.Fatalf("unknown parser: %v", err)
	}
}
