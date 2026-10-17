// Package arc verifies the TOP ARC set of a message (RFC 8617) and reads its
// ARC-Authentication-Results, so a receiver can trust the authentication
// verdict of a named intermediary that sealed it (nagus-25g; design
// docs/design/2026-10-16-promo-vision.md, "ARC trust").
//
// Why: the deals mailbox is ForwardEmail IMAP storage. ForwardEmail's MX
// verifies a message's DKIM on receipt and records the verdict in the ARC
// set it seals (i=N, d=forwardemail.net), but its STORAGE then alters the
// body, so nagus's own DKIM check of the stored copy fails ("body hash did
// not verify") although the message was genuine on arrival.
//
// What is bound by what:
//
//   - The ARC-Seal (AS) of instance N signs ONLY the ARC header fields of
//     instances 1..N (AAR, AMS, AS). It proves the sealer wrote this
//     verdict and this AMS -- not that they belong to THIS message: a seal
//     copied onto another message still verifies.
//   - The ARC-Message-Signature (AMS) of instance N signs the message's
//     header fields it lists in h= (From, To, Subject, Date, ...) and the
//     body hash bh= as the sealer computed it on receipt. Verify verifies
//     the AMS signature over those headers USING bh= AS WRITTEN, so the
//     headers of this message are the ones the sealer saw; it does not
//     recompute the body hash, which is exactly what storage broke. The
//     body is therefore NOT bound -- callers must treat it accordingly.
//
// The trust anchor is the sealer's DNS key (for ForwardEmail,
// default._domainkey.forwardemail.net, RSA 1024). Only sealers the caller
// names are accepted; lower instances are trusted only through the cv=
// values the top sealer attested. No maintained Go ARC verifier exists in
// the module's dependencies (go-msgauth v0.7.0 has DKIM and
// Authentication-Results only), so this is a small, self-contained one.
package arc

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Chain is a verified top ARC set.
type Chain struct {
	Instance   int    // i=N of the top set
	Sealer     string // the top ARC-Seal's d=, lower-cased
	AuthServID string // the top ARC-Authentication-Results' authserv-id, lower-cased
	Results    []Result
	// SealTime is the top ARC-Seal's t=.
	SealTime time.Time
	// Signed are the header names (lower-cased) the top ARC-Message-Signature
	// covers.
	Signed []string
}

// Result is one resinfo of the top ARC-Authentication-Results.
type Result struct {
	Method string            // "dkim", "spf", ...
	Value  string            // "pass", "fail", ...
	Props  map[string]string // "header.i" -> "@example.com", ...
}

// DefaultSignedHeaders must all be covered by the top AMS.
var DefaultSignedHeaders = []string{"from", "to", "subject", "date"}

// Options configure a verification.
type Options struct {
	// LookupTXT resolves a selector record; required.
	LookupTXT func(name string) ([]string, error)
	// TrustedSealers are the d= domains whose top seal is accepted.
	TrustedSealers []string
	// SignedHeaders must all be in the top AMS's h=; nil means
	// DefaultSignedHeaders.
	SignedHeaders []string
}

// ErrNoARC is a message with no ARC set at all.
var ErrNoARC = errors.New("arc: no ARC headers")

// field is one raw header field: its name and its raw value (folded, as in
// the message, continuation lines joined with CRLF).
type field struct {
	name  string
	value string
}

// headerFields splits the header block of a CRLF- or LF-terminated message.
// A first line that is a continuation, or a line with no colon, makes the
// block malformed.
func headerFields(raw []byte) ([]field, error) {
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	end := bytes.Index(raw, []byte("\n\n"))
	if end < 0 {
		end = len(raw)
	}
	var out []field
	for _, line := range strings.Split(string(raw[:end]), "\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(out) == 0 {
				return nil, errors.New("arc: header block starts with a continuation line")
			}
			out[len(out)-1].value += "\r\n" + line
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t") {
			return nil, errors.New("arc: malformed header line")
		}
		out = append(out, field{name: name, value: value})
	}
	return out, nil
}

// tags parses a DKIM-style tag list (RFC 6376 3.2). Duplicate tags or a tag
// without "=" are an error.
func tags(v string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("arc: malformed tag %q", part)
		}
		k = strings.TrimSpace(k)
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("arc: duplicate tag %q", k)
		}
		out[k] = stripWSP(val)
	}
	return out, nil
}

func stripWSP(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

// relaxed is the relaxed header canonicalization (RFC 6376 3.4.2): name
// lower-cased, the value unfolded, whitespace runs collapsed to one space,
// whitespace around the colon and at the end removed.
func relaxed(f field) string {
	v := strings.ReplaceAll(f.value, "\r\n", "")
	v = strings.Join(strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == '\t' }), " ")
	return strings.ToLower(strings.TrimSpace(f.name)) + ":" + strings.TrimSpace(v)
}

// simple is the simple header canonicalization: the field unchanged.
func simple(f field) string { return f.name + ":" + f.value }

// emptyB returns the value with the b= tag's value removed, everything
// else (other tags, their whitespace) unchanged.
func emptyB(v string) string {
	parts := strings.Split(v, ";")
	for i, p := range parts {
		k, _, ok := strings.Cut(p, "=")
		if ok && strings.TrimSpace(k) == "b" {
			parts[i] = k + "="
		}
	}
	return strings.Join(parts, ";")
}

// instanceOf reads i= from an ARC header value: the first tag, 1..50.
func instanceOf(v string) (int, error) {
	first, _, _ := strings.Cut(v, ";")
	k, val, ok := strings.Cut(first, "=")
	if !ok || strings.TrimSpace(k) != "i" {
		return 0, errors.New("arc: header does not start with i=")
	}
	n, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil || n < 1 || n > 50 {
		return 0, fmt.Errorf("arc: bad instance %q", val)
	}
	return n, nil
}

var (
	selectorRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,63}$`)
	domainRe   = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
)

// ValidDomain reports whether d is a lower-case DNS name of two or more
// labels.
func ValidDomain(d string) bool { return len(d) <= 253 && domainRe.MatchString(d) }

type set struct {
	aar, ams, as *field
}

// Verify checks the message's ARC sets, the top seal, and the top AMS over
// the message's headers. It returns the verified top set, or an error saying
// why the chain is not trusted.
func Verify(raw []byte, o Options) (*Chain, error) {
	if o.LookupTXT == nil {
		return nil, errors.New("arc: no key lookup")
	}
	fields, err := headerFields(raw)
	if err != nil {
		return nil, err
	}
	sets := map[int]*set{}
	for i := range fields {
		f := &fields[i]
		name := strings.ToLower(strings.TrimSpace(f.name))
		if name != "arc-seal" && name != "arc-message-signature" && name != "arc-authentication-results" {
			continue
		}
		n, err := instanceOf(f.value)
		if err != nil {
			return nil, err
		}
		s := sets[n]
		if s == nil {
			s = &set{}
			sets[n] = s
		}
		var slot **field
		switch name {
		case "arc-seal":
			slot = &s.as
		case "arc-message-signature":
			slot = &s.ams
		default:
			slot = &s.aar
		}
		if *slot != nil {
			return nil, fmt.Errorf("arc: duplicate %s for i=%d", f.name, n)
		}
		*slot = f
	}
	if len(sets) == 0 {
		return nil, ErrNoARC
	}
	n := len(sets)
	for i := 1; i <= n; i++ {
		s, ok := sets[i]
		if !ok {
			return nil, fmt.Errorf("arc: instances are not contiguous 1..%d", n)
		}
		if s.aar == nil || s.ams == nil || s.as == nil {
			return nil, fmt.Errorf("arc: instance %d is incomplete", i)
		}
		t, err := tags(s.as.value)
		if err != nil {
			return nil, err
		}
		want := "pass"
		if i == 1 {
			want = "none"
		}
		if strings.ToLower(t["cv"]) != want {
			return nil, fmt.Errorf("arc: instance %d has cv=%s, want %s", i, t["cv"], want)
		}
	}
	top := sets[n]
	st, err := tags(top.as.value)
	if err != nil {
		return nil, err
	}
	sealer := strings.ToLower(st["d"])
	if !slices.ContainsFunc(o.TrustedSealers, func(d string) bool { return strings.EqualFold(strings.TrimSpace(d), sealer) }) {
		return nil, fmt.Errorf("arc: top seal by %q, not a trusted sealer", sealer)
	}
	if !selectorRe.MatchString(st["s"]) || st["b"] == "" {
		return nil, errors.New("arc: top seal lacks a valid s= or b=")
	}
	ts, err := strconv.ParseInt(st["t"], 10, 64)
	if err != nil || ts <= 0 {
		return nil, errors.New("arc: top seal has no valid t=")
	}
	// 1. The seal: AAR, AMS, AS of every instance in order, the top seal
	// last with its b= emptied and no trailing CRLF.
	var in strings.Builder
	for i := 1; i <= n; i++ {
		s := sets[i]
		in.WriteString(relaxed(*s.aar) + "\r\n")
		in.WriteString(relaxed(*s.ams) + "\r\n")
		if i < n {
			in.WriteString(relaxed(*s.as) + "\r\n")
		}
	}
	in.WriteString(relaxed(field{name: top.as.name, value: emptyB(top.as.value)}))
	if err := verifySig(o.LookupTXT, st["a"], st["s"], sealer, st["b"], in.String()); err != nil {
		return nil, fmt.Errorf("arc: top seal: %w", err)
	}
	// 2. The top AMS over this message's headers, bh= as written.
	signed, err := verifyAMS(o, fields, top.ams, sealer)
	if err != nil {
		return nil, err
	}
	id, results, err := parseAAR(top.aar.value)
	if err != nil {
		return nil, err
	}
	return &Chain{Instance: n, Sealer: sealer, AuthServID: id, Results: results, SealTime: time.Unix(ts, 0).UTC(), Signed: signed}, nil
}

// verifyAMS verifies the top ARC-Message-Signature over the header fields
// it lists, selected bottom-up as in DKIM (RFC 6376 5.4.2), using its bh= as
// written (the body hash is not recomputed). It must be by the sealer and
// cover every required header.
func verifyAMS(o Options, fields []field, ams *field, sealer string) ([]string, error) {
	t, err := tags(ams.value)
	if err != nil {
		return nil, err
	}
	if strings.ToLower(t["d"]) != sealer {
		return nil, errors.New("arc: top AMS is not by the sealer")
	}
	if !selectorRe.MatchString(t["s"]) || t["b"] == "" || t["bh"] == "" || t["h"] == "" {
		return nil, errors.New("arc: top AMS lacks s=, b=, bh= or h=")
	}
	headerCanon, _, _ := strings.Cut(strings.ToLower(t["c"]), "/")
	canon := relaxed
	switch headerCanon {
	case "relaxed":
	case "", "simple":
		canon = simple
	default:
		return nil, fmt.Errorf("arc: AMS canonicalization %q", t["c"])
	}
	var names []string
	for _, h := range strings.Split(t["h"], ":") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			names = append(names, h)
		}
	}
	required := o.SignedHeaders
	if required == nil {
		required = DefaultSignedHeaders
	}
	for _, r := range required {
		if !slices.Contains(names, strings.ToLower(r)) {
			return nil, fmt.Errorf("arc: top AMS does not cover %s", r)
		}
	}
	used := make([]bool, len(fields))
	var in strings.Builder
	for _, h := range names {
		// The bottom-most unused instance of the header; none = nothing.
		for i := len(fields) - 1; i >= 0; i-- {
			if !used[i] && strings.EqualFold(strings.TrimSpace(fields[i].name), h) {
				used[i] = true
				in.WriteString(canon(fields[i]) + "\r\n")
				break
			}
		}
	}
	in.WriteString(canon(field{name: ams.name, value: emptyB(ams.value)}))
	if err := verifySig(o.LookupTXT, t["a"], t["s"], sealer, t["b"], in.String()); err != nil {
		return nil, fmt.Errorf("arc: top AMS: %w", err)
	}
	return names, nil
}

// verifySig verifies b64sig over input with the key of selector s at d.
func verifySig(lookup func(string) ([]string, error), alg, s, d, b64sig, input string) error {
	sig, err := base64.StdEncoding.DecodeString(b64sig)
	if err != nil {
		return errors.New("b= is not base64")
	}
	key, err := lookupKey(lookup, s+"._domainkey."+d)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(input))
	switch alg {
	case "rsa-sha256":
		pk, ok := key.(*rsa.PublicKey)
		if !ok || rsa.VerifyPKCS1v15(pk, crypto.SHA256, sum[:], sig) != nil {
			return errors.New("signature does not verify")
		}
	case "ed25519-sha256":
		pk, ok := key.(ed25519.PublicKey)
		if !ok || !ed25519.Verify(pk, sum[:], sig) {
			return errors.New("signature does not verify")
		}
	default:
		return fmt.Errorf("algorithm %q not accepted", alg)
	}
	return nil
}

// lookupKey fetches and parses a DKIM key record. A record restricted to
// another service (s= without email or *) is refused. The t=s flag (no
// subdomains of d in i=) does not apply: ARC signatures carry no i=.
func lookupKey(lookup func(string) ([]string, error), name string) (crypto.PublicKey, error) {
	txts, err := lookup(name)
	if err != nil {
		return nil, fmt.Errorf("key lookup %s: %w", name, err)
	}
	t, err := tags(strings.Join(txts, ""))
	if err != nil {
		return nil, err
	}
	if v, ok := t["v"]; ok && v != "DKIM1" {
		return nil, errors.New("key record version is not DKIM1")
	}
	if svc, ok := t["s"]; ok {
		okSvc := false
		for _, s := range strings.Split(svc, ":") {
			if s == "email" || s == "*" {
				okSvc = true
			}
		}
		if !okSvc {
			return nil, errors.New("key record is not for email")
		}
	}
	der, err := base64.StdEncoding.DecodeString(t["p"])
	if err != nil || len(der) == 0 {
		return nil, errors.New("key record has no usable p=")
	}
	switch k := t["k"]; k {
	case "", "rsa":
		if pub, err := x509.ParsePKIXPublicKey(der); err == nil {
			if rk, ok := pub.(*rsa.PublicKey); ok && rk.N.BitLen() >= 1024 {
				return rk, nil
			}
			return nil, errors.New("key is not an RSA key of at least 1024 bits")
		}
		rk, err := x509.ParsePKCS1PublicKey(der)
		if err != nil || rk.N.BitLen() < 1024 {
			return nil, errors.New("unparseable RSA key")
		}
		return rk, nil
	case "ed25519":
		if len(der) != ed25519.PublicKeySize {
			return nil, errors.New("bad ed25519 key")
		}
		return ed25519.PublicKey(der), nil
	default:
		return nil, fmt.Errorf("key type %q not accepted", k)
	}
}

// stripComments removes RFC 5322 comments (nested parentheses), leaving
// quoted strings intact.
func stripComments(s string) string {
	var b strings.Builder
	depth, inQuote, esc := 0, false, false
	for _, r := range s {
		switch {
		case esc:
			esc = false
			if depth == 0 {
				b.WriteRune(r)
			}
			continue
		case r == '\\':
			esc = true
			if depth == 0 {
				b.WriteRune(r)
			}
			continue
		case inQuote:
			if r == '"' {
				inQuote = false
			}
		case r == '"' && depth == 0:
			inQuote = true
		case r == '(':
			depth++
			continue
		case r == ')' && depth > 0:
			depth--
			continue
		}
		if depth == 0 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// splitUnquoted splits s at any rune in seps outside double quotes, dropping
// empty pieces when dropEmpty.
func splitUnquoted(s, seps string, dropEmpty bool) []string {
	var out []string
	var cur strings.Builder
	inQuote, esc := false, false
	flush := func() {
		if !dropEmpty || cur.Len() > 0 {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case esc:
			esc = false
		case r == '\\' && inQuote:
			esc = true
		case r == '"':
			inQuote = !inQuote
		case !inQuote && strings.ContainsRune(seps, r):
			flush()
			continue
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}

// identityProps are the properties a caller aligns on: accepted only
// unquoted and domain-shaped.
var identityProps = map[string]bool{"header.d": true, "header.i": true}

// parseAAR parses "i=N; authserv-id [version]; method=result prop=val ...;
// ...". Tokens are split outside quotes; a property repeated within one
// resinfo makes the header unparseable; header.d and header.i must be
// unquoted and domain-shaped or the resinfo is dropped.
func parseAAR(v string) (string, []Result, error) {
	v = strings.ReplaceAll(stripComments(v), "\r\n", " ")
	parts := splitUnquoted(v, ";", false)
	if len(parts) < 2 {
		return "", nil, errors.New("arc: malformed ARC-Authentication-Results")
	}
	idf := strings.Fields(parts[1])
	if len(idf) == 0 {
		return "", nil, errors.New("arc: ARC-Authentication-Results has no authserv-id")
	}
	id := strings.ToLower(idf[0])
	var out []Result
	for _, p := range parts[2:] {
		f := splitUnquoted(p, " \t", true)
		if len(f) == 0 {
			continue
		}
		m, val, ok := strings.Cut(f[0], "=")
		if !ok {
			continue
		}
		r := Result{Method: strings.ToLower(m), Value: strings.ToLower(val), Props: map[string]string{}}
		valid := true
		for _, kv := range f[1:] {
			k, pv, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			k = strings.ToLower(k)
			if _, dup := r.Props[k]; dup {
				return "", nil, fmt.Errorf("arc: property %s repeated in one result", k)
			}
			if identityProps[k] && !identityShaped(k, pv) {
				valid = false
			}
			r.Props[k] = strings.Trim(pv, `"`)
		}
		if valid {
			out = append(out, r)
		}
	}
	return id, out, nil
}

// identityShaped: header.d is a domain, header.i is "[local]@domain";
// neither may be quoted.
func identityShaped(k, v string) bool {
	if strings.ContainsAny(v, `"\`) {
		return false
	}
	v = strings.ToLower(v)
	if k == "header.d" {
		return ValidDomain(v)
	}
	at := strings.LastIndex(v, "@")
	return at >= 0 && ValidDomain(v[at+1:]) && !strings.ContainsAny(v[:at], " \t@")
}
