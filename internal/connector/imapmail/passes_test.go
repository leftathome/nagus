package imapmail

import (
	"bytes"
	"context"
	"errors"
	"net"
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

func passConnector(t *testing.T, addr string, p Parser, kr *keyring, passes AuthPasses) *Connector {
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
	// go-msgauth checks x= against the real clock, in whole seconds.
	exp := time.Now().Add(2 * time.Second)
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
	k := passKey(7, 42, raw)
	for name, other := range map[string]string{
		"other bytes":       passKey(7, 42, []byte("From: a@b\r\n\r\nbodY\r\n")),
		"other uid":         passKey(7, 43, raw),
		"other uidvalidity": passKey(8, 42, raw),
		"digit shift":       passKey(74, 2, raw),
	} {
		if other == k {
			t.Errorf("%s: same pass key", name)
		}
	}
	if passKey(7, 42, raw) != k {
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
