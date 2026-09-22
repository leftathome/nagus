package ttbcola

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/quilceda.csv is a real registry export (2026-09-21): three Quilceda
// Creek approvals and one Leonetti Cellar approval.
const fixture = "testdata/quilceda.csv"

var fixedNow = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func TestParsesTheRegistryExport(t *testing.T) {
	c := NewConnector(Config{Name: "allocation", FixturePath: fixture, Now: func() time.Time { return fixedNow }})
	raws, err := c.Fetch(context.Background())
	if err != nil || !c.FetchComplete() {
		t.Fatalf("Fetch: %v complete=%v", err, c.FetchComplete())
	}
	if len(raws) != 4 {
		t.Fatalf("got %d approvals, want 4", len(raws))
	}
	r := raws[1]
	a := r.Aspects
	if r.SourceID != "ttbcola:allocation" || r.SourceKey != "24045001000988" || r.Title != "QUILCEDA CREEK PALENGAT" || r.PriceCents != 0 {
		t.Fatalf("raw %+v", r)
	}
	if a["approval_date"] != "2024-02-15" || a["permit"] != "BW-WA-68" || a["brand"] != "QUILCEDA CREEK" ||
		a["fanciful_name"] != "PALENGAT" || a["origin"] != "WASHINGTON" || !strings.Contains(a["class_type"], "WINE") {
		t.Fatalf("aspects %v", a)
	}
	if !strings.HasSuffix(r.SourceURL, "ttbid=24045001000988") {
		t.Fatalf("detail link %q", r.SourceURL)
	}
	if raws[0].Title != "QUILCEDA CREEK" || raws[0].Aspects["fanciful_name"] != "" {
		t.Fatalf("no fanciful name: title %q", raws[0].Title)
	}
}

func TestRedesignedExportIsLoud(t *testing.T) {
	if _, err := parseCSV([]byte("id,name\n1,x\n")); err == nil {
		t.Fatal("an export with other columns must be an error")
	}
}

// registry fakes the servlet: a session cookie from the search page is required
// by the search and the export, as on the real site.
func registry(t *testing.T, csvFor map[string]string, forms *[]string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	lastBrand := ""
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/colasonline/publicSearchColasBasic.do":
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "s1", Path: "/"})
			_, _ = w.Write([]byte("<form>search</form>"))
		case "/colasonline/publicSearchColasBasicProcess.do":
			if _, err := r.Cookie("JSESSIONID"); err != nil {
				http.Error(w, "no session", http.StatusForbidden)
				return
			}
			_ = r.ParseForm()
			*forms = append(*forms, r.Form.Encode())
			lastBrand = r.Form.Get("searchCriteria.productOrFancifulName")
			if csvFor[lastBrand] == "" {
				_, _ = w.Write([]byte("No results were found that match the search criteria specified."))
				return
			}
			_, _ = w.Write([]byte("1 to 4 of 4 (Total Matching Records: 4)"))
		case "/colasonline/publicSaveSearchResultsToFile.do":
			if _, err := r.Cookie("JSESSIONID"); err != nil {
				http.Error(w, "no session", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/csv")
			if csvFor[lastBrand] == "fixture" {
				_, _ = w.Write(body)
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSearchesEachBrandInItsOwnSessionAndDedups(t *testing.T) {
	var forms []string
	srv := registry(t, map[string]string{"QUILCEDA CREEK": "fixture", "%QUILCEDA%": "fixture"}, &forms)
	defer srv.Close()
	var pauses int
	c := NewConnector(Config{Name: "a", Brands: []string{"QUILCEDA CREEK", "CAYUSE VINEYARDS", "%QUILCEDA%"},
		BaseURL: srv.URL + "/colasonline", HTTP: srv.Client(), Now: func() time.Time { return fixedNow },
		Sleep: func(context.Context, time.Duration) error { pauses++; return nil }})
	raws, err := c.Fetch(context.Background())
	if err != nil || !c.FetchComplete() {
		t.Fatalf("Fetch: %v", err)
	}
	if len(raws) != 4 {
		t.Fatalf("got %d, want 4 (the wildcard search repeats the same approvals)", len(raws))
	}
	if len(forms) != 3 {
		t.Fatalf("searches %d, want 3", len(forms))
	}
	if !strings.Contains(forms[0], "dateCompletedFrom=08%2F07%2F2026") || !strings.Contains(forms[0], "dateCompletedTo=09%2F21%2F2026") ||
		!strings.Contains(forms[0], "productNameSearchType=B") {
		t.Fatalf("form %s", forms[0])
	}
	if pauses == 0 {
		t.Fatal("requests must be paced")
	}
}

func TestRedesignedSearchPageIsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>new registry</html>"))
	}))
	defer srv.Close()
	c := NewConnector(Config{Name: "a", Brands: []string{"X"}, BaseURL: srv.URL, HTTP: srv.Client(), Pause: -1})
	if _, err := c.Fetch(context.Background()); err == nil || c.FetchComplete() {
		t.Fatal("a search page with neither results nor 'No results' must be an error")
	}
}

// --- TLS: the server sends only its leaf, as ttbonline.gov does ---

type pki struct {
	root, inter    *x509.Certificate
	rootKey, inKey *ecdsa.PrivateKey
}

func newCA(t *testing.T, cn string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, key
}

func newPKI(t *testing.T) pki {
	root, rk := newCA(t, "test root", nil, nil)
	inter, ik := newCA(t, "test intermediate", root, rk)
	return pki{root: root, inter: inter, rootKey: rk, inKey: ik}
}

func (p pki) leafServer(t *testing.T, host string) *httptest.Server {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: host},
		DNSNames: []string{host}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.inter, &key.PublicKey, p.inKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	// Leaf only: no intermediate in the chain, the ttbonline.gov misconfiguration.
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	return srv
}

func pemOf(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

func get(t *testing.T, cfg *tls.Config, u, serverName string) error {
	t.Helper()
	cfg.ServerName = serverName
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, err := hc.Get(u)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func TestTLSVerifiesALeafOnlyChainThroughTheSuppliedIntermediate(t *testing.T) {
	p := newPKI(t)
	srv := p.leafServer(t, "registry.test")
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(p.root)

	if err := get(t, tlsConfig(roots, pemOf(p.inter)), srv.URL, "registry.test"); err != nil {
		t.Fatalf("a valid chain completed by the supplied intermediate must pass: %v", err)
	}
	// The same leaf-only server without the intermediate: what plain Go sees.
	other := newPKI(t)
	if err := get(t, tlsConfig(roots, pemOf(other.inter)), srv.URL, "registry.test"); err == nil {
		t.Fatal("a chain that does not reach a trusted root must fail")
	}
	// Right chain, wrong host.
	if err := get(t, tlsConfig(roots, pemOf(p.inter)), srv.URL, "evil.test"); err == nil {
		t.Fatal("a certificate for another host must fail")
	}
	// Right chain, untrusted root.
	untrusted := x509.NewCertPool()
	untrusted.AddCert(other.root)
	if err := get(t, tlsConfig(untrusted, pemOf(p.inter)), srv.URL, "registry.test"); err == nil {
		t.Fatal("a chain to an untrusted root must fail")
	}
}

func TestEmbeddedIntermediateIsTheEntrustIssuer(t *testing.T) {
	b, _ := pem.Decode(entrustIntermediatePEM)
	if b == nil {
		t.Fatal("embedded intermediate is not PEM")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject.CommonName != "Entrust OV TLS Issuing RSA CA 2" || c.Issuer.CommonName != "Sectigo Public Server Authentication Root R46" || !c.IsCA {
		t.Fatalf("subject %q issuer %q", c.Subject.CommonName, c.Issuer.CommonName)
	}
	if c.NotAfter.Before(time.Date(2027, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expires %v", c.NotAfter)
	}
}
