// Package arctest seals synthetic ARC sets for tests: a sealer key (Ed25519
// or RSA), the DNS record for it, and a REAL ARC-Message-Signature (signed
// over the listed header fields, with a real relaxed body hash) and
// ARC-Seal. Test-only; production code never imports it.
package arctest

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Sealer seals ARC sets as Domain with selector Selector.
type Sealer struct {
	Domain, Selector string
	Key              ed25519.PrivateKey
	RSA              *rsa.PrivateKey // when set, signs rsa-sha256 instead
	// Time is the seal's t=; 0 omits t= entirely.
	Time int64
	// Signed is the AMS h= list; empty = From:To:Subject:Date:Message-ID.
	Signed string
}

// NewSealer returns an Ed25519 sealer with a fresh key and t=1.
func NewSealer(domain string) (*Sealer, error) {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Sealer{Domain: domain, Selector: "arc1", Key: k, Time: 1}, nil
}

// NewRSASealer returns an RSA-2048 sealer with a fresh key and t=1.
func NewRSASealer(domain string) (*Sealer, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &Sealer{Domain: domain, Selector: "arcrsa", RSA: k, Time: 1}, nil
}

// TXTName is where the sealer's key is published.
func (s *Sealer) TXTName() string { return s.Selector + "._domainkey." + s.Domain }

// TXT is the key record's value.
func (s *Sealer) TXT() string {
	if s.RSA != nil {
		der, _ := x509.MarshalPKIXPublicKey(&s.RSA.PublicKey)
		return "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
	}
	return "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(s.Key.Public().(ed25519.PublicKey))
}

func (s *Sealer) alg() string {
	if s.RSA != nil {
		return "rsa-sha256"
	}
	return "ed25519-sha256"
}

func (s *Sealer) sign(input string) string {
	sum := sha256.Sum256([]byte(input))
	if s.RSA != nil {
		sig, err := rsa.SignPKCS1v15(rand.Reader, s.RSA, crypto.SHA256, sum[:])
		if err != nil {
			panic(err)
		}
		return base64.StdEncoding.EncodeToString(sig)
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(s.Key, sum[:]))
}

type field struct{ name, value string }

func relaxed(f field) string {
	v := strings.ReplaceAll(f.value, "\r\n", "")
	v = strings.Join(strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == '\t' }), " ")
	return strings.ToLower(strings.TrimSpace(f.name)) + ":" + strings.TrimSpace(v)
}

func split(msg string) ([]field, string) {
	head, body, _ := strings.Cut(strings.ReplaceAll(msg, "\r\n", "\n"), "\n\n")
	var fields []field
	for _, line := range strings.Split(head, "\n") {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(fields) > 0 {
			fields[len(fields)-1].value += "\r\n" + line
			continue
		}
		n, v, _ := strings.Cut(line, ":")
		fields = append(fields, field{n, v})
	}
	return fields, body
}

// bodyHash is the relaxed body hash (RFC 6376 3.4.4, 3.7).
func bodyHash(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lead := strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")
		lines[i] = strings.Join(strings.FieldsFunc(l, func(r rune) bool { return r == ' ' || r == '\t' }), " ")
		if lead && lines[i] != "" {
			lines[i] = " " + lines[i]
		}
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	c := ""
	if len(lines) > 0 {
		c = strings.Join(lines, "\r\n") + "\r\n"
	}
	sum := sha256.Sum256([]byte(c))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Seal prepends the next ARC set to msg: an ARC-Authentication-Results by
// authserv with results, an ARC-Message-Signature over the listed headers
// and the body, and an ARC-Seal with cv over every set.
func (s *Sealer) Seal(msg, authserv, results, cv string) string {
	fields, body := split(msg)
	sets := map[int][3]field{}
	for _, f := range fields {
		idx, ok := map[string]int{"arc-authentication-results": 0, "arc-message-signature": 1, "arc-seal": 2}[strings.ToLower(f.name)]
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(f.value, ";")
		_, iv, _ := strings.Cut(first, "=")
		i, _ := strconv.Atoi(strings.TrimSpace(iv))
		set := sets[i]
		set[idx] = f
		sets[i] = set
	}
	n := len(sets) + 1
	signed := s.Signed
	if signed == "" {
		signed = "From:To:Subject:Date:Message-ID"
	}
	aar := field{"ARC-Authentication-Results", fmt.Sprintf(" i=%d; %s;\r\n %s", n, authserv, results)}
	amsNoSig := fmt.Sprintf(" i=%d; a=%s; c=relaxed/relaxed; d=%s; s=%s;\r\n h=%s; bh=%s; b=", n, s.alg(), s.Domain, s.Selector, signed, bodyHash(body))
	var hin strings.Builder
	used := make([]bool, len(fields))
	for _, h := range strings.Split(signed, ":") {
		for i := len(fields) - 1; i >= 0; i-- {
			if !used[i] && strings.EqualFold(strings.TrimSpace(fields[i].name), h) {
				used[i] = true
				hin.WriteString(relaxed(fields[i]) + "\r\n")
				break
			}
		}
	}
	hin.WriteString(relaxed(field{"ARC-Message-Signature", amsNoSig}))
	ams := field{"ARC-Message-Signature", amsNoSig + s.sign(hin.String())}
	t := ""
	if s.Time != 0 {
		t = fmt.Sprintf(" t=%d;", s.Time)
	}
	asNoSig := fmt.Sprintf(" i=%d; a=%s;%s cv=%s; d=%s; s=%s; b=", n, s.alg(), t, cv, s.Domain, s.Selector)
	var in strings.Builder
	for i := 1; i < n; i++ {
		for _, f := range sets[i] {
			in.WriteString(relaxed(f) + "\r\n")
		}
	}
	in.WriteString(relaxed(aar) + "\r\n" + relaxed(ams) + "\r\n" + relaxed(field{"ARC-Seal", asNoSig}))
	return "ARC-Seal:" + asNoSig + s.sign(in.String()) + "\r\nARC-Message-Signature:" + ams.value +
		"\r\nARC-Authentication-Results:" + aar.value + "\r\n" + msg
}
