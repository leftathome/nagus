package imapmail

// Regression tests from the security review of nagus !35 (rv35). Each one
// reproduced an exploit before its fix.

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func dealConn(t *testing.T, senders []string, kr *keyring, mut ...func(*Config)) *Connector {
	t.Helper()
	cfg := Config{Name: "deals", Host: "h", Port: "1", TLS: "none", Username: "u", Password: "p",
		Senders: senders, Parser: &recorder{}, Now: func() time.Time { return now }}
	if kr != nil {
		cfg.LookupTXT = kr.lookupTXT
	} else {
		cfg.LookupTXT = func(string) ([]string, error) { return nil, nil }
	}
	for _, m := range mut {
		m(&cfg)
	}
	c, err := NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// C1: an unsigned message whose topmost Authentication-Results header was
// written by the SENDER (ForwardEmail writes none, so nothing sits above it)
// must not be accepted. A-R trust is off unless a source opts in.
func TestForgedTopmostAuthResultsIsNotTrusted(t *testing.T) {
	c := dealConn(t, []string{"household@gmail.com"}, nil)
	raw := eml("forged-1@x", "household@gmail.com", "S", "mx1.forwardemail.net; dkim=pass header.d=gmail.com")
	if msg, why := c.verify([]byte(raw)); why == "" {
		t.Fatalf("EXPLOIT: unsigned mail with a forged topmost A-R accepted as %s", msg.From)
	}
}

// C1: the A-R path still exists, for a receiving MX that writes the header,
// but only when a source opts in.
func TestAuthResultsTrustIsOptIn(t *testing.T) {
	c := dealConn(t, nil, nil, func(c *Config) { c.From = sender; c.Senders = nil; c.TrustAuthResults = true })
	if _, why := c.verify([]byte(eml("ar-1@x", sender, "S", passAR("lastbottlewines.com")))); why != "" {
		t.Fatalf("opted-in A-R trust refused: %s", why)
	}
}

// C2: a second From header prepended above a genuinely signed message. DKIM
// verifies the bottom From; the message must not be credited to the top one.
func TestDuplicateFromIsRefused(t *testing.T) {
	kr := newKeyring(t)
	signed := kr.sign(eml("atk-1@x", "attacker@example.org", "A", ""), "example.org")
	c := dealConn(t, []string{"household@example.org"}, kr)
	if msg, why := c.verify([]byte("From: Household <household@example.org>\r\n" + signed)); why == "" {
		t.Fatalf("EXPLOIT: duplicate-From spoof accepted as %s", msg.From)
	}
	// ... and even when the bottom From is the allowlisted one, a message
	// with two From fields is ambiguous and refused.
	c2 := dealConn(t, []string{"attacker@example.org"}, kr)
	if _, why := c2.verify([]byte("From: Someone <someone@example.org>\r\n" + signed)); why == "" {
		t.Fatal("two From fields accepted")
	}
}

// C2: duplicate Sender, Subject and Message-ID fields are refused too.
func TestDuplicateIdentityHeadersAreRefused(t *testing.T) {
	kr := newKeyring(t)
	c := dealConn(t, []string{"household@example.org"}, kr)
	base := kr.sign(eml("dup-1@x", "household@example.org", "A", ""), "example.org")
	if _, why := c.verify([]byte(base)); why != "" {
		t.Fatalf("the clean message was refused: %s", why)
	}
	for _, h := range []string{"Message-ID: <other@x>", "Subject: other", "Sender: a@example.org\r\nSender: b@example.org"} {
		if _, why := c.verify([]byte(h + "\r\n" + base)); why == "" {
			t.Errorf("duplicate %q accepted", strings.SplitN(h, ":", 2)[0])
		}
	}
}

// N1: a forward is credited to the FORWARDER, the address DKIM verified. The
// original sender named inside the forwarded text is unauthenticated and is
// only recorded as a claim.
func TestForwardIsCreditedToTheForwarder(t *testing.T) {
	kr := newKeyring(t)
	c := dealConn(t, []string{"caspar@agents.example.org", "human@example.org"}, kr,
		func(c *Config) { c.Forwarders = []string{"fwd@example.net"} })
	msg, why := c.verify([]byte(kr.sign(fwd("f-1@x", "fwd@example.net", "caspar@agents.example.org"), "example.net")))
	if why != "" {
		t.Fatal(why)
	}
	if msg.From != "fwd@example.net" || msg.ForwardedBy != "fwd@example.net" || msg.ForwardedFrom != "caspar@agents.example.org" {
		t.Fatalf("EXPLOIT: forward credited to %q (claimed original %q)", msg.From, msg.ForwardedFrom)
	}
}

// N2: IMAP SEARCH FROM is a substring match, so a stranger whose DISPLAY NAME
// holds an allowlisted address matches it. The parsed envelope address is
// checked before any body is fetched.
func TestDisplayNameMatchIsNotFetched(t *testing.T) {
	big := strings.Repeat("padding line\r\n", 400)
	raw := "Message-ID: <dn-1@x>\r\nFrom: \"human@example.org\" <stranger@evil.example>\r\nTo: deals@example.net\r\nSubject: x\r\n" +
		"Date: Tue, 22 Sep 2026 08:00:00 +0000\r\n\r\n" + big
	addr := server(t, map[string]time.Time{raw: now.Add(-time.Hour)})
	host, port, _ := net.SplitHostPort(addr)
	var obs []Observation
	c, err := NewConnector(Config{Name: "deals", Host: host, Port: port, TLS: "none", Username: "deals@totally.apocryph.al",
		Password: "pw", Senders: []string{"human@example.org"}, Parser: &recorder{}, MaxBytes: 1024,
		Observe: func(o Observation) { obs = append(obs, o) }, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Reason != SkipUnknownSender {
		t.Fatalf("observations %+v: a display-name match must be an unknown sender, never reach the body fetch", obs)
	}
}

// N5: the time nagus orders and retains by is the server's INTERNALDATE, not
// the sender-written Date header.
func TestReceivedIsTheInternalDate(t *testing.T) {
	kr := newKeyring(t)
	arrived := now.Add(-3 * time.Hour).Truncate(time.Second)
	raw := strings.Replace(eml("d-1@x", "human@example.org", "A", ""), "Date: Tue, 22 Sep 2026 08:00:00", "Date: Fri, 01 Jan 2100 00:00:00", 1)
	addr := server(t, map[string]time.Time{kr.sign(raw, "example.org"): arrived})
	host, port, _ := net.SplitHostPort(addr)
	rec := &recorder{}
	c, err := NewConnector(Config{Name: "deals", Host: host, Port: port, TLS: "none", Username: "deals@totally.apocryph.al",
		Password: "pw", Senders: []string{"human@example.org"}, Parser: rec, LookupTXT: kr.lookupTXT, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.seen) != 1 || !rec.seen[0].Received.Equal(arrived) {
		t.Fatalf("received %v, want the INTERNALDATE %v", rec.seen, arrived)
	}
}
