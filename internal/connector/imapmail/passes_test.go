package imapmail

import (
	"bytes"
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// memPasses is an in-test AuthPasses.
type memPasses struct {
	mu     sync.Mutex
	passes map[[2]string]string
	getErr error
	puts   int
}

func newMemPasses() *memPasses { return &memPasses{passes: map[[2]string]string{}} }

func (m *memPasses) GetAuthPass(_ context.Context, src, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return "", false, m.getErr
	}
	p, ok := m.passes[[2]string{src, key}]
	return p, ok, nil
}

func (m *memPasses) PutAuthPass(_ context.Context, src, key, path string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.passes[[2]string{src, key}] = path
	m.puts++
	return nil
}

func (m *memPasses) PurgeAuthPasses(context.Context, string, time.Time) error { return nil }

func (m *memPasses) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.passes)
}

// signUntil is keyring.sign with a signature expiry (x=).
func (k *keyring) signUntil(raw, domain string, exp time.Time) string {
	var out bytes.Buffer
	err := dkim.Sign(&out, strings.NewReader(raw), &dkim.SignOptions{Domain: domain, Selector: "s1", Signer: k.key(domain), Expiration: exp})
	if err != nil {
		k.t.Fatal(err)
	}
	return out.String()
}

func passConnector(t *testing.T, addr string, p Parser, kr *keyring, passes AuthPasses, mut ...func(*Config)) *Connector {
	t.Helper()
	if p == nil {
		p = &recorder{}
	}
	host, port, _ := net.SplitHostPort(addr)
	cfg := Config{
		Name: "lastbottle", Host: host, Port: port, TLS: "none",
		Username: "deals@totally.apocryph.al", Password: "pw", From: sender, Parser: p,
		LookupTXT: kr.lookupTXT, Now: func() time.Time { return now }, Logf: t.Logf,
	}
	if passes != nil {
		cfg.Passes = passes
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

func fetchSubjects(t *testing.T, c *Connector) []string {
	t.Helper()
	rec := &recorder{}
	c.cfg.Parser = rec
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range rec.seen {
		got = append(got, m.Subject)
	}
	return got
}

// nagus-aoq: Gmail signs with x= = t + 7 days and the lookback window is 14,
// so a genuine message that verified on arrival must stay accepted after its
// signature expires -- across a restart too -- while a connector with no
// memory refuses it (the bug).
func TestAPassSurvivesTheSignatureExpiring(t *testing.T) {
	kr := newKeyring(t)
	// go-msgauth checks x= against the real clock, in whole seconds (it has
	// no clock option), so this one test waits for a real expiry. The
	// margin covers a loaded CI node reaching the first fetch.
	exp := time.Now().Add(5 * time.Second)
	addr := server(t, map[string]time.Time{
		kr.signUntil(eml("exp-1@lb", sender, "Expiring offer", ""), "lastbottlewines.com", exp): now.Add(-time.Hour),
	})
	passes := newMemPasses()
	c := passConnector(t, addr, nil, kr, passes)
	if got := fetchSubjects(t, c); len(got) != 1 {
		t.Fatalf("before expiry: parser saw %v, want the signed offer", got)
	}
	if passes.len() != 1 {
		t.Fatalf("the verified message was not remembered: %d passes", passes.len())
	}

	time.Sleep(time.Until(exp.Truncate(time.Second).Add(1100 * time.Millisecond)))

	if got := fetchSubjects(t, passConnector(t, addr, nil, kr, nil)); len(got) != 0 {
		t.Fatalf("without a pass memory an expired signature should be refused (the bug this fixes), parser saw %v", got)
	}
	if got := fetchSubjects(t, c); len(got) != 1 {
		t.Fatalf("after expiry: parser saw %v, want the offer still accepted", got)
	}
	// A restart: a new connector over the same persisted passes.
	if got := fetchSubjects(t, passConnector(t, addr, nil, kr, passes)); len(got) != 1 {
		t.Fatalf("after a restart: parser saw %v, want the offer still accepted", got)
	}
	if passes.puts != 1 {
		t.Fatalf("a remembered pass was recorded again: %d puts", passes.puts)
	}
}

// A signature already expired when the message is first read never passes,
// so it is never remembered.
func TestAnExpiredSignatureIsNeverRecorded(t *testing.T) {
	kr := newKeyring(t)
	addr := server(t, map[string]time.Time{
		kr.signUntil(eml("old@lb", sender, "Replayed offer", ""), "lastbottlewines.com", time.Now().Add(-time.Hour)): now.Add(-time.Hour),
	})
	passes := newMemPasses()
	if got := fetchSubjects(t, passConnector(t, addr, nil, kr, passes)); len(got) != 0 {
		t.Fatalf("parser saw %v, want an expired-at-first-sight signature refused", got)
	}
	if passes.len() != 0 {
		t.Fatalf("a refused message was remembered as a pass: %d", passes.len())
	}
}

// A store that cannot be read does not accept anything: the message is
// verified as if never seen.
func TestAPassStoreErrorFallsBackToVerification(t *testing.T) {
	kr := newKeyring(t)
	addr := server(t, map[string]time.Time{
		kr.sign(eml("ok@lb", sender, "Signed offer", ""), "lastbottlewines.com"): now.Add(-time.Hour),
		eml("spoof@evil", sender, "Unsigned spoof", ""):                          now.Add(-time.Hour),
	})
	passes := newMemPasses()
	passes.getErr = errors.New("database is down")
	got := fetchSubjects(t, passConnector(t, addr, nil, kr, passes))
	if len(got) != 1 || got[0] != "Signed offer" {
		t.Fatalf("parser saw %v, want only the verified message", got)
	}
}

// A remembered pass belongs to exact bytes at an exact place: a different
// message at the same UID, or the same bytes after UIDVALIDITY changes, has
// another key.
func TestPassKeyIsBoundToTheBytesAndTheUID(t *testing.T) {
	raw := []byte("From: a@b\r\n\r\nbody\r\n")
	k := passKey("salt", 7, 42, raw)
	for name, other := range map[string]string{
		"other bytes":       passKey("salt", 7, 42, []byte("From: a@b\r\n\r\nbodY\r\n")),
		"other uid":         passKey("salt", 7, 43, raw),
		"other uidvalidity": passKey("salt", 8, 42, raw),
		"digit shift":       passKey("salt", 74, 2, raw),
		"other config":      passKey("tlas", 7, 42, raw),
	} {
		if other == k {
			t.Errorf("%s: same pass key", name)
		}
	}
	if passKey("salt", 7, 42, raw) != k {
		t.Fatal("pass key is not deterministic")
	}
}

// A stored path that is not an authentication path is ignored: the message
// is verified again.
func TestAnUnknownStoredPathIsIgnored(t *testing.T) {
	kr := newKeyring(t)
	spoof := eml("spoof@evil", sender, "Unsigned spoof", "")
	addr := server(t, map[string]time.Time{spoof: now.Add(-time.Hour)})
	passes := &forcedPasses{path: "bogus"}
	if got := fetchSubjects(t, passConnector(t, addr, nil, kr, passes)); len(got) != 0 {
		t.Fatalf("parser saw %v, want an unknown stored path ignored", got)
	}
	// And a real path is honoured as such (the store is trusted).
	passes.path = PathDKIM
	if got := fetchSubjects(t, passConnector(t, addr, nil, kr, passes)); len(got) != 1 {
		t.Fatalf("parser saw %v, want a stored dkim pass honoured", got)
	}
}

// forcedPasses answers every lookup with path.
type forcedPasses struct{ path string }

func (f *forcedPasses) GetAuthPass(context.Context, string, string) (string, bool, error) {
	return f.path, true, nil
}

func (f *forcedPasses) PutAuthPass(context.Context, string, string, string, time.Time) error {
	return nil
}

func (f *forcedPasses) PurgeAuthPasses(context.Context, string, time.Time) error { return nil }

// Changing who is trusted re-verifies everything: a pass recorded under the
// old config is not honoured (revoking AR/ARC trust or a sender takes effect
// on the next poll).
func TestAuthConfigFingerprintCoversTrust(t *testing.T) {
	base := Config{From: "a@x.com", Senders: []string{"b@y.com", "c@z.com"}, TrustAuthResults: true}
	fp := authConfigFingerprint(base)
	reordered := base
	reordered.Senders = []string{"c@z.com", "b@y.com"}
	if authConfigFingerprint(reordered) != fp {
		t.Fatal("sender order changed the fingerprint")
	}
	// Adding a submitter or a household forwarder revokes nothing and must
	// not throw every pass away (that would refuse week-old Gmail again).
	grown := base
	grown.Senders = append(slices.Clone(base.Senders), "d@w.com")
	grown.Forwarders = []string{"f@g.com"}
	if authConfigFingerprint(grown) != fp {
		t.Fatal("adding a sender or forwarder changed the fingerprint")
	}
	// List entries are length-prefixed: ["a,b"] is not ["a", "b"].
	one, two := base, base
	one.TrustARCSealers, two.TrustARCSealers = []string{"a,b"}, []string{"a", "b"}
	if authConfigFingerprint(one) == authConfigFingerprint(two) {
		t.Fatal("list entries are ambiguous")
	}
	for name, mut := range map[string]func(*Config){
		"auth-results off": func(c *Config) { c.TrustAuthResults = false },
		"dkim domain":      func(c *Config) { c.DKIMDomain = "other.com" },
		"from":             func(c *Config) { c.From = "z@x.com" },
		"arc sealer":       func(c *Config) { c.TrustARCSealers = []string{"s.com"} },
		"arc authserv":     func(c *Config) { c.TrustARCAuthServ = []string{"s.com"} },
		"arc recipients":   func(c *Config) { c.TrustARCRecipients = []string{"r@s.com"} },
		"arc age":          func(c *Config) { c.ARCMaxAge = time.Hour },
		"authserv":         func(c *Config) { c.TrustedAuthServ = []string{"mx.example"} },
	} {
		c := base
		mut(&c)
		if authConfigFingerprint(c) == fp {
			t.Errorf("%s: fingerprint unchanged", name)
		}
	}
}

// End to end: a pass recorded by one config is not honoured by a connector
// whose trust changed; the message is verified again.
func TestAPassIsNotHonouredAfterTrustChanges(t *testing.T) {
	kr := newKeyring(t)
	// Passes only through the opt-in Authentication-Results path.
	addr := server(t, map[string]time.Time{
		eml("ar-1@lb", sender, "AR offer", passAR("lastbottlewines.com")): now.Add(-time.Hour),
	})
	passes := newMemPasses()
	trusting := passConnector(t, addr, nil, kr, passes, func(c *Config) { c.TrustAuthResults = true })
	if got := fetchSubjects(t, trusting); len(got) != 1 || passes.len() != 1 {
		t.Fatalf("trusting A-R: parser saw %v, %d passes", got, passes.len())
	}
	// The operator turns A-R trust off: the remembered auth_results pass
	// must not keep the message accepted.
	if got := fetchSubjects(t, passConnector(t, addr, nil, kr, passes)); len(got) != 0 {
		t.Fatalf("after revoking A-R trust: parser saw %v, want refused", got)
	}
}

// A stored pass replaces only authentication: every other check still runs.
func TestAPassDoesNotSkipTheOtherChecks(t *testing.T) {
	kr := newKeyring(t)
	const household = "forwarder@example.org"
	noID := strings.Replace(eml("x@lb", sender, "No message id", ""), "Message-ID: <x@lb>\r\n", "", 1)
	dupSubject := strings.Replace(eml("dup@lb", sender, "Dup subject", ""), "Subject: Dup subject\r\n", "Subject: Dup subject\r\nSubject: Another\r\n", 1)
	for name, raw := range map[string]string{
		"sender not listed":         eml("gone@x", "former@elsewhere.example", "Former sender", ""),
		"no message-id":             noID,
		"duplicate identity header": dupSubject,
		"forward of another sender": fwd("fwd-x@gmail", household, "news@somewhere.com"),
	} {
		t.Run(name, func(t *testing.T) {
			addr := server(t, map[string]time.Time{raw: now.Add(-time.Hour)})
			c := passConnector(t, addr, nil, kr, &forcedPasses{path: PathDKIM}, func(c *Config) { c.Forwarders = []string{household} })
			if got := fetchSubjects(t, c); len(got) != 0 {
				t.Fatalf("parser saw %v, want refused despite a stored pass", got)
			}
		})
	}
	// Control: the same forced pass does accept a well-formed message.
	addr := server(t, map[string]time.Time{eml("ok@lb", sender, "Well formed", ""): now.Add(-time.Hour)})
	if got := fetchSubjects(t, passConnector(t, addr, nil, kr, &forcedPasses{path: PathDKIM})); len(got) != 1 {
		t.Fatalf("control: parser saw %v", got)
	}
}
