package arc

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/arc/arctest"
)

// Placeholders only: example domains.
const (
	sealerDomain = "seal.example.net"
	relayDomain  = "relay.example.org"
)

const baseMsg = "From: News <news@sender.example.com>\r\nTo: <reader@example.com>\r\nSubject: Weekly deals\r\n" +
	"Date: Fri, 16 Oct 2026 15:32:47 +0000\r\nMessage-ID: <m1@sender.example.com>\r\n\r\nbody line\r\n"

type dns map[string]string

func (d dns) lookup(name string) ([]string, error) {
	if v, ok := d[name]; ok {
		return []string{v}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func sealers(t *testing.T, rsa bool) (*arctest.Sealer, *arctest.Sealer, dns) {
	t.Helper()
	newS := arctest.NewSealer
	if rsa {
		newS = arctest.NewRSASealer
	}
	top, err := newS(sealerDomain)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := arctest.NewSealer(relayDomain)
	if err != nil {
		t.Fatal(err)
	}
	return top, relay, dns{top.TXTName(): top.TXT(), relay.TXTName(): relay.TXT()}
}

// chain seals msg twice by the relay and once (on top) by top.
func chain(top, relay *arctest.Sealer, msg string) string {
	m := relay.Seal(msg, "mx.relay.example.org", "dkim=pass header.i=@sender.example.com", "none")
	m = relay.Seal(m, "mx.relay.example.org", "dkim=pass header.i=@sender.example.com", "pass")
	return top.Seal(m, "mx1.seal.example.net", "dkim=pass (good) header.i=@sender.example.com header.s=s1", "pass")
}

func TestVerifyValidChain(t *testing.T) {
	for _, useRSA := range []bool{false, true} {
		top, relay, d := sealers(t, useRSA)
		o := Options{LookupTXT: d.lookup, TrustedSealers: []string{sealerDomain}}
		msg := chain(top, relay, baseMsg)
		c, err := Verify([]byte(msg), o)
		if err != nil {
			t.Fatalf("rsa=%v: %v", useRSA, err)
		}
		if c.Instance != 3 || c.Sealer != sealerDomain || c.AuthServID != "mx1.seal.example.net" || c.SealTime.Unix() != 1 ||
			len(c.Results) != 1 || c.Results[0].Props["header.i"] != "@sender.example.com" || !strings.Contains(strings.Join(c.Signed, ":"), "subject") {
			t.Fatalf("chain %+v", c)
		}
		// The body is intentionally NOT bound: storage rewrites it.
		if _, err := Verify([]byte(strings.Replace(msg, "body line", "BODY REWRITTEN BY STORAGE", 1)), o); err != nil {
			t.Fatalf("modified body: %v", err)
		}
		if _, err := Verify([]byte(strings.ReplaceAll(msg, "\r\n", "\n")), o); err != nil {
			t.Fatalf("LF: %v", err)
		}
	}
}

func TestVerifyRefuses(t *testing.T) {
	top, relay, d := sealers(t, false)
	o := Options{LookupTXT: d.lookup, TrustedSealers: []string{sealerDomain}}
	valid := chain(top, relay, baseMsg)
	forger, _ := arctest.NewSealer(sealerDomain) // same domain and selector, a key not in DNS
	forger.Selector = top.Selector
	other, _ := arctest.NewSealer("other.example.org")
	d[other.TXTName()] = other.TXT()
	noT, _ := arctest.NewSealer(sealerDomain)
	noT.Selector, noT.Time = "not", 0
	d[noT.TXTName()] = noT.TXT()
	noSubject, _ := arctest.NewSealer(sealerDomain)
	noSubject.Selector, noSubject.Signed = "nosubj", "From:To:Date"
	d[noSubject.TXTName()] = noSubject.TXT()
	wrongCV := relay.Seal(relay.Seal(baseMsg, "mx.relay.example.org", "dkim=pass", "none"), "mx.relay.example.org", "dkim=pass", "fail")

	for name, msg := range map[string]string{
		"swapped subject":   strings.Replace(valid, "Subject: Weekly deals", "Subject: attacker text", 1),
		"swapped from":      strings.Replace(valid, "From: News <news@sender.example.com>", "From: News <news@attacker.example.org>", 1),
		"swapped to":        strings.Replace(valid, "To: <reader@example.com>", "To: <victim@example.com>", 1),
		"added second to":   strings.Replace(valid, "Subject: Weekly deals", "To: <victim@example.com>\r\nSubject: Weekly deals", 1),
		"tampered aar":      strings.Replace(valid, "header.s=s1", "header.s=s2", 1),
		"tampered seal tag": strings.Replace(valid, "t=1; cv=pass; d="+sealerDomain, "t=2; cv=pass; d="+sealerDomain, 1),
		"forged seal":       chain(forger, relay, baseMsg),
		"untrusted sealer":  chain(other, relay, baseMsg),
		"missing t=":        chain(noT, relay, baseMsg),
		"ams lacks subject": chain(noSubject, relay, baseMsg),
		"cv=fail below":     top.Seal(wrongCV, "mx1.seal.example.net", "dkim=pass header.i=@sender.example.com", "pass"),
		"i=1 cv not none":   top.Seal(relay.Seal(baseMsg, "mx", "dkim=pass", "pass"), "mx1.seal.example.net", "dkim=pass", "pass"),
		"missing instance":  strings.NewReplacer("ARC-Seal: i=1;", "ARC-Seal: i=4;", "ARC-Message-Signature: i=1;", "ARC-Message-Signature: i=4;", "ARC-Authentication-Results: i=1;", "ARC-Authentication-Results: i=4;").Replace(valid),
		"incomplete set":    strings.Replace(valid, "ARC-Message-Signature: i=1;", "X-Removed: i=1;", 1),
		"duplicate header":  "ARC-Authentication-Results: i=3; mx1.seal.example.net; dkim=pass header.i=@sender.example.com\r\n" + valid,
		"bad instance":      "ARC-Seal: i=x; cv=none\r\n" + valid,
		"continuation 1st":  " folded\r\n" + valid,
		"no colon line":     "NotAHeader\r\n" + valid,
		"no key in dns":     strings.ReplaceAll(valid, "s="+top.Selector+"; b=", "s=nokey; b="),
	} {
		if _, err := Verify([]byte(msg), o); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	o.TrustedSealers = []string{"someone.example.org"}
	if _, err := Verify([]byte(valid), o); err == nil {
		t.Error("an untrusted sealer list verified")
	}
	if _, err := Verify([]byte(baseMsg), o); !errors.Is(err, ErrNoARC) {
		t.Errorf("no ARC: %v", err)
	}
}

// A key published for another service is refused.
func TestKeyServiceFlag(t *testing.T) {
	top, relay, d := sealers(t, false)
	msg := chain(top, relay, baseMsg)
	d[top.TXTName()] = top.TXT() + "; s=other"
	if _, err := Verify([]byte(msg), Options{LookupTXT: d.lookup, TrustedSealers: []string{sealerDomain}}); err == nil {
		t.Fatal("a non-email key verified")
	}
	d[top.TXTName()] = top.TXT() + "; s=email:other"
	if _, err := Verify([]byte(msg), Options{LookupTXT: d.lookup, TrustedSealers: []string{sealerDomain}}); err != nil {
		t.Fatalf("an email key refused: %v", err)
	}
}

func TestParseAAR(t *testing.T) {
	id, rs, err := parseAAR(" i=3; mx1.example.net 1;\r\n" + ` dkim=pass header.i=@a.example.com header.b="x;y";` +
		` spf=pass (mx1.example.net: domain of a (nested) b designates 1.2.3.4) smtp.mailfrom="x@a.example.com";` +
		` dmarc=pass (p=REJECT arc=pass) header.from=a.example.com`)
	if err != nil || id != "mx1.example.net" {
		t.Fatalf("id %q err %v", id, err)
	}
	got := map[string]string{}
	for _, r := range rs {
		got[r.Method] = r.Value
	}
	if got["dkim"] != "pass" || got["spf"] != "pass" || got["dmarc"] != "pass" {
		t.Fatalf("results %+v", rs)
	}
	// A quoted value cannot smuggle a second header.i, and a repeated
	// property makes the header unparseable.
	_, rs, err = parseAAR(` i=3; mx1.example.net; dkim=pass header.i=@attacker.example.com header.s="x header.i=@promo.example.com"`)
	if err != nil || len(rs) != 1 || rs[0].Props["header.i"] != "@attacker.example.com" {
		t.Fatalf("quoted smuggle: %+v %v", rs, err)
	}
	if _, _, err := parseAAR(` i=3; mx1.example.net; dkim=pass header.i=@attacker.example.com header.i=@promo.example.com`); err == nil {
		t.Fatal("a repeated property parsed")
	}
	// header.i / header.d must be unquoted and domain-shaped.
	for _, v := range []string{`header.i="@promo.example.com"`, `header.i=promo.example.com`, `header.d=not_a_domain`, `header.d="promo.example.com"`} {
		if _, rs, err := parseAAR(` i=3; mx1.example.net; dkim=pass ` + v); err != nil || len(rs) != 0 {
			t.Errorf("%s: kept %+v (err %v)", v, rs, err)
		}
	}
}
