package imapmail

// Regression tests from the security RE-review of nagus !35 (rv35b).

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testMailbox = "deals@example.org"

func fetchConn(t *testing.T, addr string, mut func(*Config)) *Connector {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	cfg := Config{Name: "deals", Host: host, Port: port, TLS: "none", Username: testMailbox, Password: "pw",
		Senders: []string{"household@example.org"}, Parser: &recorder{}, CountIgnored: true, Now: func() time.Time { return now }}
	mut(&cfg)
	c, err := NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// NEW-4: the spoofer writes the Message-ID, so two spoofs sharing one must
// still be two observations; and a duplicate-From message that names an
// allowlisted address is an identity attack, counted with the unverified.
func TestUnverifiedObservationsAreKeyedByUID(t *testing.T) {
	kr := newKeyring(t)
	attack := "From: Household <household@example.org>\r\n" + kr.sign(eml("atk@x", "attacker@example.org", "A", ""), "example.org")
	addr := serverFor(t, testMailbox, map[string]time.Time{
		eml("same@evil.example", "household@example.org", "one", ""):                     now.Add(-3 * time.Hour),
		eml("same@evil.example", "household@example.org", "two", "") + "second body\r\n": now.Add(-time.Hour),
		attack: now.Add(-2 * time.Hour),
	})
	var obs []Observation
	c := fetchConn(t, addr, func(c *Config) {
		c.LookupTXT = kr.lookupTXT
		c.Observe = func(o Observation) { obs = append(obs, o) }
	})
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, o := range obs {
		if o.Reason != SkipUnverified {
			t.Errorf("observation %+v: want unverified (a duplicate From naming an allowlisted address counts too)", o)
		}
		if strings.Contains(o.Key, "evil.example") || !strings.HasPrefix(o.Key, "uid:") {
			t.Errorf("observation key %q is sender-controlled", o.Key)
		}
		keys[o.Key] = true
	}
	if len(obs) != 3 || len(keys) != 3 {
		t.Fatalf("%d observations, %d distinct keys; want 3 and 3: %+v", len(obs), len(keys), obs)
	}
}

// Nit: DKIM alignment has a public-suffix floor. A signature by "org" (or
// "co.uk") is not a pass for example.org.
func TestAlignmentHasAPublicSuffixFloor(t *testing.T) {
	for _, c := range []struct {
		d, domain string
		want      bool
	}{
		{"example.org", "example.org", true},
		{"example.org", "mail.example.org", true},
		{"mail.example.org", "example.org", false},
		{"org", "example.org", false},
		{"co.uk", "shop.example.co.uk", false},
		{"example.co.uk", "shop.example.co.uk", true},
		{"com", "gmail.com", false},
		{"", "example.org", false},
		{"other.example.org", "sub.example.org", false},
	} {
		if got := aligned(c.d, c.domain); got != c.want {
			t.Errorf("aligned(%q, %q) = %v, want %v", c.d, c.domain, got, c.want)
		}
	}
	kr := newKeyring(t)
	c := dealConn(t, []string{"household@example.org"}, kr)
	if msg, why := c.verify([]byte(kr.sign(eml("tld@x", "household@example.org", "A", ""), "org"))); why == "" {
		t.Fatalf("EXPLOIT: a signature by the TLD accepted as %s", msg.From)
	}
}

// NEW-10: a DKIM key lookup that never answers cannot hang a poll, and its
// failure is not remembered (it is temporary).
func TestDKIMLookupTimesOut(t *testing.T) {
	kr := newKeyring(t)
	raw := kr.sign(eml("slow@x", "household@example.org", "A", ""), "example.org")
	block := make(chan struct{})
	defer close(block)
	c := dealConn(t, []string{"household@example.org"}, nil, func(c *Config) {
		c.DNSTimeout = 30 * time.Millisecond
		c.LookupTXT = func(string) ([]string, error) { <-block; return nil, nil }
	})
	done := make(chan string, 1)
	go func() { _, why := c.verify([]byte(raw)); done <- why }()
	select {
	case why := <-done:
		if why == "" {
			t.Fatal("accepted without a key")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verify hung on a DNS lookup that never answers")
	}
}

// NEW-10: an unverified message is verified once, not on every poll for the
// whole lookback window.
func TestUnverifiedVerdictIsRemembered(t *testing.T) {
	kr := newKeyring(t)
	// Signed by a domain with no published key for household's domain: a
	// permanent failure.
	addr := serverFor(t, testMailbox, map[string]time.Time{
		kr.sign(eml("bad@x", "household@example.org", "A", ""), "evil.example"): now.Add(-time.Hour),
	})
	var lookups atomic.Int64
	var obs []Observation
	c := fetchConn(t, addr, func(c *Config) {
		c.LookupTXT = func(n string) ([]string, error) { lookups.Add(1); return kr.lookupTXT(n) }
		c.Observe = func(o Observation) { obs = append(obs, o) }
	})
	for i := 0; i < 3; i++ {
		if _, err := c.Fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := lookups.Load(); n != 1 {
		t.Fatalf("%d DKIM lookups over 3 polls, want 1 (the verdict is remembered)", n)
	}
	if len(obs) != 3 || obs[2].Reason != SkipUnverified || obs[2].Key != obs[0].Key {
		t.Fatalf("each poll still reports it, under the same key: %+v", obs)
	}
}

// NEW-6: the connector reports In-Reply-To / References.
func TestReplyHeadersAreReported(t *testing.T) {
	kr := newKeyring(t)
	c := dealConn(t, []string{"household@example.org"}, kr)
	fresh := eml("f@x", "household@example.org", "A", "")
	if m, why := c.verify([]byte(kr.sign(fresh, "example.org"))); why != "" || m.Reply {
		t.Fatalf("fresh message: reply=%v why=%q", m.Reply, why)
	}
	for _, h := range []string{"In-Reply-To: <older@example.org>", "References: <older@example.org>"} {
		if m, why := c.verify([]byte(kr.sign(h+"\r\n"+fresh, "example.org"))); why != "" || !m.Reply {
			t.Fatalf("%s: reply=%v why=%q", h, m.Reply, why)
		}
	}
}
