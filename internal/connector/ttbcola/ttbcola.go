// Package ttbcola reads the TTB Public COLA Registry: label approvals as a
// leading release signal (nagus-0ek).
//
// Allocation-only producers (Quilceda Creek, Cayuse, Leonetti, ...) have no
// public store to watch, but every label they bottle needs a Certificate of
// Label Approval first, and the registry publishes approvals within days. This
// connector searches the registry by brand name over a lookback window and
// emits one unpriced listing per approval. It is a public government registry
// that needs no login; requests are few (three per brand per poll) and paced.
//
// Mechanics (a legacy servlet, 2026-09-21): GET the basic search page for a
// session cookie, POST the search form, then GET the session's
// "save search results to file" export, which is CSV:
//
//	TTB ID,Permit No.,Serial Number,Completed Date,Fanciful Name,Brand Name,Origin,Origin Desc,Class/Type,Class/Type Desc
//	'26012001000650',BW-WA-67,260001,01/15/2026,,LEONETTI CELLAR,07,WASHINGTON,88,...
//
// TLS: ttbonline.gov serves only its leaf certificate and omits the issuing
// intermediate (Entrust OV TLS Issuing RSA CA 2), so standard verification
// fails with "unable to get local issuer certificate" while browsers recover by
// fetching the intermediate from the certificate's AIA URL. Go does no AIA
// fetching, so this package embeds that intermediate (fetched from the leaf's
// AIA URL, http://crt.sectigo.com/EntrustOVTLSIssuingRSACA2.crt, valid to
// 2027-12-10) and verifies the server's chain through it to the SYSTEM roots,
// hostname included. Verification is never skipped: see verifyChain.
package ttbcola

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/connector/webfetch"
	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the connector family; a configured source is "ttbcola:<Name>".
const SourceID = "ttbcola"

// DefaultBaseURL is the registry root.
const DefaultBaseURL = "https://ttbonline.gov/colasonline"

// DefaultLookbackDays is the approval window searched on each poll.
const DefaultLookbackDays = 45

// DefaultPause separates requests: gentle on a legacy servlet.
const DefaultPause = 5 * time.Second

//go:embed entrust_ov_tls_issuing_rsa_ca2.pem
var entrustIntermediatePEM []byte

// Config configures one registry source.
type Config struct {
	Name string
	// Brands are exact brand names as registered ("QUILCEDA CREEK"); a "%"
	// wildcard is honored by the registry ("%QUILCEDA%").
	Brands       []string
	LookbackDays int
	BaseURL      string
	FixturePath  string
	// HTTP overrides the client (tests). Its Jar is replaced per brand.
	HTTP  *http.Client
	Pause time.Duration
	Sleep func(context.Context, time.Duration) error
	Now   func() time.Time
	Logf  func(string, ...any)
}

// Connector implements listing.Connector.
type Connector struct {
	cfg          Config
	mu           sync.Mutex
	lastComplete bool
}

// NewConnector fills defaults. A zero Pause means DefaultPause; negative, none.
func NewConnector(cfg Config) *Connector {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.LookbackDays <= 0 {
		cfg.LookbackDays = DefaultLookbackDays
	}
	if cfg.Pause == 0 {
		cfg.Pause = DefaultPause
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: TLSConfig(nil),
		}}
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Connector{cfg: cfg}
}

// TLSConfig verifies a server chain through the embedded intermediate to
// roots (nil = the system pool). crypto/tls cannot be handed extra
// intermediates for its built-in verification, so the documented alternative
// is used: disable only the BUILT-IN check and perform the same full
// verification in VerifyConnection -- chain to a trusted root, validity
// dates, key usage and the server name. A failure aborts the handshake.
func TLSConfig(roots *x509.CertPool) *tls.Config {
	return tlsConfig(roots, entrustIntermediatePEM)
}

func tlsConfig(roots *x509.CertPool, intermediatePEM []byte) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Replaced, not skipped: VerifyConnection below verifies every
		// handshake in full (verifyChain).
		InsecureSkipVerify: true, //nolint:gosec // full verification in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyChain(cs, roots, intermediatePEM)
		},
	}
}

func verifyChain(cs tls.ConnectionState, roots *x509.CertPool, intermediatePEM []byte) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("ttbcola: server presented no certificate")
	}
	inter := x509.NewCertPool()
	if !inter.AppendCertsFromPEM(intermediatePEM) {
		return errors.New("ttbcola: embedded intermediate does not parse")
	}
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		DNSName:       cs.ServerName,
		Roots:         roots, // nil: system roots
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("ttbcola: certificate verification: %w", err)
	}
	return nil
}

// SourceID returns "ttbcola:<Name>".
func (c *Connector) SourceID() string { return SourceID + ":" + c.cfg.Name }

// FetchComplete reports whether the last Fetch searched every brand.
func (c *Connector) FetchComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastComplete
}

// Fetch searches each brand and returns one Raw per label approval.
func (c *Connector) Fetch(ctx context.Context) ([]listing.Raw, error) {
	c.setComplete(false)
	now := c.cfg.Now()
	if c.cfg.FixturePath != "" {
		out, err := c.fixture(now)
		if err == nil {
			c.setComplete(true)
		}
		return out, err
	}
	if len(c.cfg.Brands) == 0 {
		return nil, errors.New("ttbcola: no brands configured")
	}
	seen := map[string]bool{}
	var out []listing.Raw
	for i, brand := range c.cfg.Brands {
		if i > 0 {
			if err := c.pause(ctx); err != nil {
				return nil, err
			}
		}
		body, err := c.search(ctx, brand, now)
		if err != nil {
			return nil, fmt.Errorf("ttbcola %s: brand %q: %w", c.cfg.Name, brand, err)
		}
		rows, err := parseCSV(body)
		if err != nil {
			return nil, fmt.Errorf("ttbcola %s: brand %q: %w", c.cfg.Name, brand, err)
		}
		for _, r := range rows {
			if seen[r.ttbID] {
				continue
			}
			seen[r.ttbID] = true
			out = append(out, c.raw(r, now))
		}
	}
	if c.cfg.Logf != nil {
		c.cfg.Logf("ttbcola %s: %d approvals in the last %d days across %d brands", c.cfg.Name, len(out), c.cfg.LookbackDays, len(c.cfg.Brands))
	}
	c.setComplete(true)
	return out, nil
}

// search runs one brand search in a fresh session and returns the CSV export.
func (c *Connector) search(ctx context.Context, brand string, now time.Time) ([]byte, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	hc := *c.cfg.HTTP
	hc.Jar = jar
	if _, err := c.do(ctx, &hc, http.MethodGet, c.cfg.BaseURL+"/publicSearchColasBasic.do", nil); err != nil {
		return nil, fmt.Errorf("open search: %w", err)
	}
	if err := c.pause(ctx); err != nil {
		return nil, err
	}
	form := url.Values{
		"searchCriteria.dateCompletedFrom":     {now.AddDate(0, 0, -c.cfg.LookbackDays).Format("01/02/2006")},
		"searchCriteria.dateCompletedTo":       {now.Format("01/02/2006")},
		"searchCriteria.productOrFancifulName": {brand},
		"searchCriteria.productNameSearchType": {"B"},
		"searchCriteria.classTypeFrom":         {""},
		"searchCriteria.classTypeTo":           {""},
		"searchCriteria.originCode":            {""},
	}
	page, err := c.do(ctx, &hc, http.MethodPost, c.cfg.BaseURL+"/publicSearchColasBasicProcess.do?action=search", form)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if bytes.Contains(page, []byte("No results were found")) {
		return nil, nil
	}
	if !bytes.Contains(page, []byte("Total Matching Records")) {
		return nil, errors.New("search page has neither results nor 'No results' (registry redesigned?)")
	}
	if err := c.pause(ctx); err != nil {
		return nil, err
	}
	return c.do(ctx, &hc, http.MethodGet, c.cfg.BaseURL+"/publicSaveSearchResultsToFile.do?path=/publicSearchColasBasicProcess", nil)
}

func (c *Connector) do(ctx context.Context, hc *http.Client, method, u string, form url.Values) ([]byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webfetch.DefaultUserAgent)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: HTTP %d", method, u, resp.StatusCode)
	}
	return b, nil
}

type approval struct {
	ttbID, permit, serial, date, fanciful, brand, origin, classType string
}

var wantHeader = []string{"TTB ID", "Permit No.", "Serial Number", "Completed Date", "Fanciful Name", "Brand Name", "Origin", "Origin Desc", "Class/Type", "Class/Type Desc"}

// parseCSV reads the registry export; nil input means no results.
func parseCSV(b []byte) ([]approval, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("export csv: %w", err)
	}
	if len(recs) == 0 || len(recs[0]) < len(wantHeader) {
		return nil, errors.New("export has no header (registry redesigned?)")
	}
	for i, h := range wantHeader {
		if strings.TrimSpace(recs[0][i]) != h {
			return nil, fmt.Errorf("export column %d is %q, want %q (registry redesigned?)", i, recs[0][i], h)
		}
	}
	var out []approval
	for _, rec := range recs[1:] {
		if len(rec) < len(wantHeader) {
			continue
		}
		f := func(i int) string { return strings.TrimSpace(rec[i]) }
		a := approval{ttbID: strings.Trim(f(0), "'"), permit: f(1), serial: f(2), date: f(3), fanciful: f(4),
			brand: f(5), origin: f(7), classType: f(9)}
		if a.ttbID == "" {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (c *Connector) raw(a approval, now time.Time) listing.Raw {
	title := a.brand
	if a.fanciful != "" {
		title += " " + a.fanciful
	}
	aspects := map[string]string{
		"brand":      a.brand,
		"ttb_id":     a.ttbID,
		"permit":     a.permit,
		"class_type": a.classType,
		"origin":     a.origin,
	}
	if a.fanciful != "" {
		aspects["fanciful_name"] = a.fanciful
	}
	if t, err := time.Parse("01/02/2006", a.date); err == nil {
		aspects["approval_date"] = t.Format("2006-01-02")
	}
	return listing.Raw{
		SourceID:  c.SourceID(),
		SourceKey: a.ttbID,
		SourceURL: c.cfg.BaseURL + "/viewColaDetails.do?action=publicDisplaySearchBasic&ttbid=" + url.QueryEscape(a.ttbID),
		Title:     title,
		Body:      strings.TrimSpace(a.classType + " label approved " + a.date + ", origin " + a.origin + ", permit " + a.permit),
		Aspects:   aspects,
		SeenAt:    now,
	}
}

func (c *Connector) fixture(now time.Time) ([]listing.Raw, error) {
	b, err := os.ReadFile(c.cfg.FixturePath)
	if err != nil {
		return nil, err
	}
	rows, err := parseCSV(b)
	if err != nil {
		return nil, err
	}
	out := make([]listing.Raw, 0, len(rows))
	for _, r := range rows {
		out = append(out, c.raw(r, now))
	}
	return out, nil
}

func (c *Connector) pause(ctx context.Context) error {
	if c.cfg.Pause <= 0 {
		return nil
	}
	return c.cfg.Sleep(ctx, c.cfg.Pause)
}

func (c *Connector) setComplete(v bool) {
	c.mu.Lock()
	c.lastComplete = v
	c.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
