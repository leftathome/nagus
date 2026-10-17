package imapmail

import (
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/arc/arctest"
)

const (
	arcSealerDomain = "seal.example.net"
	arcAuthserv     = "mx1.seal.example.net"
	// arcRecipient is the To of eml(): the mailbox's own address.
	arcRecipient = "deals@totally.apocryph.al"
)

// sealedAt is the test sealer's t=; a message received a minute later is
// recent.
var sealedAt = time.Unix(1, 0).UTC()

var receivedAt = sealedAt.Add(time.Minute)

// arcConnector is a connector for sender with optional ARC trust; DNS
// answers the sender's DKIM key and every sealer's.
func arcConnector(t *testing.T, kr *keyring, trust bool, sealers ...*arctest.Sealer) *Connector {
	t.Helper()
	cfg := Config{
		Name: "promo", Host: "h", Username: "u", Password: "p", Senders: []string{sender}, Parser: &recorder{},
		LookupTXT: func(name string) ([]string, error) {
			for _, s := range sealers {
				if name == s.TXTName() {
					return []string{s.TXT()}, nil
				}
			}
			return kr.lookupTXT(name)
		}, Logf: t.Logf,
	}
	if trust {
		cfg.TrustARCSealers, cfg.TrustARCAuthServ, cfg.TrustARCRecipients = []string{arcSealerDomain}, []string{arcAuthserv}, []string{arcRecipient}
	}
	c, err := NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// storedCopy is a genuine signed message as mailbox storage leaves it: the
// receiving MX's sealed verdict on top, then the body rewritten (DKIM body
// hash broken).
func storedCopy(t *testing.T, kr *keyring, relay, top *arctest.Sealer, msg, authserv, results string) string {
	t.Helper()
	signed := kr.sign(msg, "lastbottlewines.com")
	sealed := relay.Seal(signed, "mx.relay.example.org", "dkim=pass header.i=@lastbottlewines.com", "none")
	sealed = top.Seal(sealed, authserv, results, "pass")
	return strings.Replace(sealed, "$29", "$ 29", 1)
}

func goodResults() string {
	return "dkim=pass (good) header.i=@lastbottlewines.com header.s=s1; spf=pass smtp.mailfrom=x@lastbottlewines.com"
}

func TestARCTrust(t *testing.T) {
	kr := newKeyring(t)
	top, _ := arctest.NewSealer(arcSealerDomain)
	relay, _ := arctest.NewSealer("relay.example.org")
	top.Time = sealedAt.Unix()
	msg := eml("stored@lb", sender, "Offer", "")
	stored := storedCopy(t, kr, relay, top, msg, arcAuthserv, goodResults())

	// Opted out: exactly as before, the stored copy fails direct DKIM.
	if _, why := arcConnector(t, kr, false, top, relay).verifyAt([]byte(stored), receivedAt); why != whyNoDKIM {
		t.Fatalf("opt-out source accepted the stored copy: %q", why)
	}
	on := arcConnector(t, kr, true, top, relay)
	before := AuthStats()
	if m, why := on.verifyAt([]byte(stored), receivedAt); why != "" || m.From != sender {
		t.Fatalf("opted-in source refused a valid ARC chain: %q", why)
	}
	if AuthStats()[PathARC] != before[PathARC]+1 {
		t.Fatal("the ARC path was not counted")
	}
	// The body is not bound (storage rewrote it): an attacker's body on a
	// genuine seal still passes -- the headers below are what bind.
	if _, why := on.verifyAt([]byte(strings.Replace(stored, "Today only", "Attacker body", 1)), receivedAt); why != "" {
		t.Fatalf("a rewritten body refused: %q", why)
	}
	// Direct DKIM still takes the DKIM path.
	if _, why := on.verifyAt([]byte(kr.sign(eml("direct@lb", sender, "Offer", ""), "lastbottlewines.com")), receivedAt); why != "" {
		t.Fatalf("direct DKIM refused: %q", why)
	}
	if st := AuthStats(); st[PathDKIM] != before[PathDKIM]+1 {
		t.Fatalf("paths %v (before %v)", st, before)
	}

	forger, _ := arctest.NewSealer(arcSealerDomain) // same domain and selector, its own key
	forger.Selector, forger.Time = top.Selector, top.Time
	other, _ := arctest.NewSealer("other.example.org")
	other.Time = top.Time
	noT, _ := arctest.NewSealer(arcSealerDomain)
	noT.Selector, noT.Time = "not", 0
	stale, _ := arctest.NewSealer(arcSealerDomain)
	stale.Selector, stale.Time = "stale", receivedAt.Add(-2*time.Hour).Unix()
	future, _ := arctest.NewSealer(arcSealerDomain)
	future.Selector, future.Time = "future", receivedAt.Add(time.Hour).Unix()
	onAll := arcConnector(t, kr, true, top, relay, other, noT, stale, future)
	otherTo := strings.Replace(msg, "To: deals@totally.apocryph.al", "To: subscriber@example.org", 1)
	coRecipient := strings.Replace(msg, "To: deals@totally.apocryph.al", "To: attacker@example.org, deals@totally.apocryph.al", 1)
	group := strings.Replace(msg, "To: deals@totally.apocryph.al", "To: team: attacker@example.org, deals@totally.apocryph.al;", 1)
	emptyGroup := strings.Replace(msg, "To: deals@totally.apocryph.al", "To: undisclosed-recipients:;", 1)
	twoTo := strings.Replace(msg, "Subject: Offer", "To: deals@totally.apocryph.al\r\nSubject: Offer", 1)
	for name, m := range map[string]string{
		"swapped subject":        strings.Replace(stored, "Subject: Offer", "Subject: Attacker text", 1),
		"swapped from name":      strings.Replace(stored, "From: Offers <", "From: Attacker <", 1),
		"to not a recipient":     storedCopy(t, kr, relay, top, otherTo, arcAuthserv, goodResults()),
		"duplicate to":           storedCopy(t, kr, relay, top, twoTo, arcAuthserv, goodResults()),
		"co-recipient in to":     storedCopy(t, kr, relay, top, coRecipient, arcAuthserv, goodResults()),
		"group of two in to":     storedCopy(t, kr, relay, top, group, arcAuthserv, goodResults()),
		"empty group in to":      storedCopy(t, kr, relay, top, emptyGroup, arcAuthserv, goodResults()),
		"stale seal":             storedCopy(t, kr, relay, stale, msg, arcAuthserv, goodResults()),
		"future seal":            storedCopy(t, kr, relay, future, msg, arcAuthserv, goodResults()),
		"missing t=":             storedCopy(t, kr, relay, noT, msg, arcAuthserv, goodResults()),
		"authserv not trusted":   storedCopy(t, kr, relay, top, msg, "mx.evil.example.org", goodResults()),
		"dkim pass other domain": storedCopy(t, kr, relay, top, msg, arcAuthserv, "dkim=pass header.i=@evil.example.org"),
		"dkim pass bare tld":     storedCopy(t, kr, relay, top, msg, arcAuthserv, "dkim=pass header.d=com"),
		"header.b mismatch":      storedCopy(t, kr, relay, top, msg, arcAuthserv, `dkim=pass header.i=@lastbottlewines.com header.b="zzzzzzzz"`),
		"header.s mismatch":      storedCopy(t, kr, relay, top, msg, arcAuthserv, "dkim=pass header.i=@lastbottlewines.com header.s=s9"),
		"property overwrite":     storedCopy(t, kr, relay, top, msg, arcAuthserv, `dkim=pass header.i=@evil.example.org header.s="x header.i=@lastbottlewines.com"`),
		"quoted header.i":        storedCopy(t, kr, relay, top, msg, arcAuthserv, `dkim=pass header.i="@lastbottlewines.com"`),
		"forged seal":            storedCopy(t, kr, relay, forger, msg, arcAuthserv, goodResults()),
		"untrusted sealer":       storedCopy(t, kr, relay, other, msg, arcAuthserv, goodResults()),
		"no arc":                 strings.Replace(kr.sign(msg, "lastbottlewines.com"), "$29", "$ 29", 1),
	} {
		if _, why := onAll.verifyAt([]byte(m), receivedAt); why != whyNoDKIM {
			t.Errorf("%s: %q, want refused", name, why)
		}
	}
	// A group of exactly the mailbox reduces to that one address.
	single := strings.Replace(msg, "To: deals@totally.apocryph.al", "To: deals: deals@totally.apocryph.al;", 1)
	if _, why := onAll.verifyAt([]byte(storedCopy(t, kr, relay, top, single, arcAuthserv, goodResults())), receivedAt); why != "" {
		t.Errorf("one-member group refused: %q", why)
	}
	// No receive time (no INTERNALDATE): ARC cannot be judged recent.
	if _, why := on.verify([]byte(stored)); why != whyNoDKIM {
		t.Errorf("no receive time: %q", why)
	}
}

func TestARCConfigValidation(t *testing.T) {
	base := Config{Name: "x", Host: "h", Username: "u", Password: "p", Senders: []string{sender}, Parser: &recorder{}}
	all := func(c *Config) {
		c.TrustARCSealers, c.TrustARCAuthServ, c.TrustARCRecipients = []string{"seal.example.net"}, []string{"mx1.seal.example.net"}, []string{arcRecipient}
	}
	for name, mutate := range map[string]func(*Config){
		"sealers only":  func(c *Config) { c.TrustARCSealers = []string{"seal.example.net"} },
		"authserv only": func(c *Config) { c.TrustARCAuthServ = []string{"mx1.seal.example.net"} },
		"no recipients": func(c *Config) { all(c); c.TrustARCRecipients = nil },
		"bad recipient": func(c *Config) { all(c); c.TrustARCRecipients = []string{"not an address"} },
		"bad sealer":    func(c *Config) { all(c); c.TrustARCSealers = []string{"not a domain"} },
		"bare tld":      func(c *Config) { all(c); c.TrustARCSealers = []string{"net"} },
		"negative age":  func(c *Config) { all(c); c.ARCMaxAge = -time.Second },
	} {
		c := base
		mutate(&c)
		if _, err := NewConnector(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := base
	c.TrustARCSealers, c.TrustARCAuthServ, c.TrustARCRecipients = []string{" Seal.Example.NET "}, []string{"MX1.seal.example.net"}, []string{"Deals@Totally.Apocryph.al"}
	conn, err := NewConnector(c)
	if err != nil || conn.cfg.TrustARCSealers[0] != "seal.example.net" || conn.cfg.TrustARCAuthServ[0] != "mx1.seal.example.net" || conn.cfg.TrustARCRecipients[0] != arcRecipient {
		t.Fatalf("normalised config: %v %+v", err, conn)
	}
}

// header.b must be a prefix of at least 8 characters of a signature by the
// same domain (a consistency check).
func TestNamesASignature(t *testing.T) {
	sigs := []dkimSig{{d: "sender.example.com", s: "s1", b: "abcdefghijklmnop"}}
	for _, tc := range []struct {
		props map[string]string
		d     string
		want  bool
	}{
		{map[string]string{}, "sender.example.com", true},
		{map[string]string{"header.b": "abcdefgh"}, "sender.example.com", true},
		{map[string]string{"header.b": "abcdefg"}, "sender.example.com", false},
		{map[string]string{"header.b": "a"}, "sender.example.com", false},
		{map[string]string{"header.b": "abcdefgX"}, "sender.example.com", false},
		{map[string]string{"header.s": "s1"}, "sender.example.com", true},
		{map[string]string{"header.s": "s2"}, "sender.example.com", false},
		{map[string]string{"header.b": "abcdefgh"}, "other.example.com", false},
	} {
		if got := namesASignature(tc.props, tc.d, sigs); got != tc.want {
			t.Errorf("namesASignature(%v, %s) = %v", tc.props, tc.d, got)
		}
	}
}
