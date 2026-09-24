package imapmail

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-msgauth/dkim"

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
		if got := c.dkimVerified([]string{ar}, "lastbottlewines.com"); got != want {
			t.Errorf("dkimVerified(%q) = %v, want %v", ar, got, want)
		}
	}
	if c.dkimVerified(nil, "lastbottlewines.com") {
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

// --- DKIM verified by nagus itself (nagus-zsi) --------------------------------

// keyring signs as any domain and answers the DKIM DNS query for it.
type keyring struct {
	t    *testing.T
	keys map[string]ed25519.PrivateKey
}

func newKeyring(t *testing.T) *keyring { return &keyring{t: t, keys: map[string]ed25519.PrivateKey{}} }

func (k *keyring) key(domain string) ed25519.PrivateKey {
	if _, ok := k.keys[domain]; !ok {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			k.t.Fatal(err)
		}
		k.keys[domain] = priv
	}
	return k.keys[domain]
}

// sign returns raw with a DKIM-Signature by domain (selector s1) prepended.
func (k *keyring) sign(raw, domain string) string {
	var out bytes.Buffer
	err := dkim.Sign(&out, strings.NewReader(raw), &dkim.SignOptions{Domain: domain, Selector: "s1", Signer: k.key(domain)})
	if err != nil {
		k.t.Fatal(err)
	}
	return out.String()
}

func (k *keyring) lookupTXT(name string) ([]string, error) {
	domain, ok := strings.CutPrefix(name, "s1._domainkey.")
	if priv, has := k.keys[domain]; ok && has {
		pub := priv.Public().(ed25519.PublicKey)
		return []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func signedConnector(t *testing.T, addr string, p Parser, kr *keyring, forwarders ...string) *Connector {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	c, err := NewConnector(Config{Name: "lastbottle", Host: host, Port: port, TLS: "none",
		Username: "deals@totally.apocryph.al", Password: "pw", From: sender, Parser: p,
		Forwarders: forwarders, LookupTXT: kr.lookupTXT, Now: func() time.Time { return now }, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ForwardEmail writes no Authentication-Results header at all -- only ARC
// sets -- so a genuine message is accepted on its own verified signature.
func TestAcceptsAVerifiedSignatureWithoutAuthenticationResults(t *testing.T) {
	kr := newKeyring(t)
	recent := now.Add(-time.Hour)
	tampered := strings.Replace(kr.sign(eml("tamper@lb", sender, "Tampered", ""), "lastbottlewines.com"), "$29", "$9", 1)
	addr := server(t, map[string]time.Time{
		kr.sign(eml("sig-1@lb", sender, "Signed offer", "",
			"ARC-Authentication-Results: i=1; evil.example; dkim=pass header.d=lastbottlewines.com"), "lastbottlewines.com"): recent,
		// the body changed after signing
		tampered: recent,
		// signed, but by a domain that does not vouch for this sender
		kr.sign(eml("wrongd@evil", sender, "Wrong signer", ""), "evil.example"): recent,
		// unsigned, and a forged ARC verdict is not trusted
		eml("arc@evil", sender, "Forged ARC", "",
			"ARC-Authentication-Results: i=1; mx1.forwardemail.net; dkim=pass header.d=lastbottlewines.com"): recent,
	})
	rec := &recorder{}
	if _, err := signedConnector(t, addr, rec, kr).Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.seen) != 1 || rec.seen[0].Subject != "Signed offer" {
		var got []string
		for _, m := range rec.seen {
			got = append(got, m.Subject)
		}
		t.Fatalf("parser saw %v, want only the message with a valid aligned signature", got)
	}
}

// fwd builds a Gmail-style hand forward of a sender's message.
func fwd(id, forwarder, origFrom string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Message-ID: <%s>\r\nFrom: Household <%s>\r\nTo: deals@totally.apocryph.al\r\nSubject: Fwd: Todays offer\r\n", id, forwarder)
	b.WriteString("Date: Tue, 22 Sep 2026 09:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString("\r\n---------- Forwarded message ---------\r\n")
	fmt.Fprintf(&b, "From: Last Bottle <%s>\r\nDate: Tue, Sep 22, 2026 at 8:00 AM\r\nSubject: Todays offer\r\nTo: <%s>\r\n\r\n", origFrom, forwarder)
	b.WriteString("Today only: 2019 Test Cabernet $29 (reg $59)\r\n")
	return b.String()
}

func TestAcceptsAHouseholdForwardOfTheSendersMail(t *testing.T) {
	kr := newKeyring(t)
	const household = "forwarder@example.org"
	recent := now.Add(-time.Hour)
	addr := server(t, map[string]time.Time{
		kr.sign(fwd("fwd-1@gmail", household, sender), "example.org"): recent,
		// a forward of a DIFFERENT sender's mail is not this source's
		kr.sign(fwd("fwd-2@gmail", household, "news@somewhere.com"), "example.org"): recent,
		// claims to be the forwarder without the forwarder domain's signature
		fwd("fwd-3@evil", household, sender):                          recent,
		kr.sign(fwd("fwd-4@evil", household, sender), "evil.example"): recent,
		// a forwarder nobody declared
		kr.sign(fwd("fwd-5@gmail", "stranger@example.org", sender), "example.org"): recent,
	})
	rec := &recorder{}
	raws, err := signedConnector(t, addr, rec, kr, household).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.seen) != 1 || len(raws) != 1 {
		t.Fatalf("saw %d messages / %d offers, want only the verified forward of the sender", len(rec.seen), len(raws))
	}
	m, r := rec.seen[0], raws[0]
	if m.From != sender || m.ForwardedBy != household || !strings.Contains(m.Text, "2019 Test Cabernet $29") {
		t.Fatalf("message %+v", m)
	}
	if r.SourceID != "imap:lastbottle" || r.Aspects["mail_forwarded_by"] != household || r.Aspects["mail_message_id"] != "fwd-1@gmail" {
		t.Fatalf("raw %+v", r)
	}
	// without the forwarder declared, the same mailbox yields nothing
	rec2 := &recorder{}
	if _, err := signedConnector(t, addr, rec2, kr).Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec2.seen) != 0 {
		t.Fatalf("undeclared forwarder accepted: %d", len(rec2.seen))
	}
}

func TestForwardedFrom(t *testing.T) {
	for text, want := range map[string]string{
		"hi\n\n---------- Forwarded message ---------\nFrom: Wine.com <winecom@e.wine.com>\nDate: x\n": "winecom@e.wine.com",
		"> Begin forwarded message:\n>\n> From: \"Last Bottle\" <Offers@LastBottleWines.com>\n":        "offers@lastbottlewines.com",
		"From: not-a-forward@example.com\n":                       "",
		"---------- Forwarded message ---------\nFrom: garbage\n": "",
	} {
		if got := forwardedFrom(text); got != want {
			t.Errorf("forwardedFrom(%q) = %q, want %q", text, got, want)
		}
	}
	if got := forwardedFrom(htmlText(`<div>---------- Forwarded message ---------<br>From: <strong>Wine.com</strong> <span>&lt;winecom@e.wine.com&gt;</span><br></div>`)); got != "winecom@e.wine.com" {
		t.Errorf("html forward: %q", got)
	}
}

func TestForwarderValidation(t *testing.T) {
	base := Config{Name: "x", Host: "h", Username: "u", Password: "p", From: sender, Parser: &recorder{}}
	for _, bad := range [][]string{{"not-an-address"}, {sender}} {
		c := base
		c.Forwarders = bad
		if _, err := NewConnector(c); err == nil {
			t.Errorf("forwarders %v: want an error", bad)
		}
	}
}

// TestRealCapturedMessage verifies a real message end to end -- its real DKIM
// signature against real DNS -- without committing personal mail to the repo.
// Opt in:
//
//	NAGUS_IMAP_REAL_EML=/path/msg.eml NAGUS_IMAP_REAL_FROM=winecom@e.wine.com \
//	NAGUS_IMAP_REAL_FORWARDER=<forwarding address> go test -run RealCaptured ./internal/connector/imapmail
func TestRealCapturedMessage(t *testing.T) {
	path, from := os.Getenv("NAGUS_IMAP_REAL_EML"), os.Getenv("NAGUS_IMAP_REAL_FROM")
	if path == "" || from == "" {
		t.Skip("set NAGUS_IMAP_REAL_EML and NAGUS_IMAP_REAL_FROM to run against a real captured message")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fw []string
	if f := os.Getenv("NAGUS_IMAP_REAL_FORWARDER"); f != "" {
		fw = []string{f}
	}
	c, err := NewConnector(Config{Name: "real", Host: "h", Username: "u", Password: "p", From: from,
		Forwarders: fw, Parser: &recorder{}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	m, why := c.verify(raw)
	if why != "" {
		t.Fatalf("rejected: %s", why)
	}
	t.Logf("accepted: from=%s forwarded_by=%s subject=%q text=%dB html=%dB", m.From, m.ForwardedBy, m.Subject, len(m.Text), len(m.HTML))
}
