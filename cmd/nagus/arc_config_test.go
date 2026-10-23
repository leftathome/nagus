package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
)

// ARC trust is per source, opt-in, and must agree across deal sources.
func TestARCTrustConfig(t *testing.T) {
	t.Setenv("NAGUS_IMAP_HOST", "imap.example.net")
	t.Setenv("NAGUS_IMAP_USERNAME", "deals@example.net")
	t.Setenv("NAGUS_IMAP_PASSWORD", "x")
	o := categoryOpts{logf: t.Logf, promo: promoRuntimeFromEnv(nil)}
	s := promoSource("promo-newegg", "newegg", "hdd", "placeholder@newegg.com")
	s.IMAPTrustARCSealers = []string{"forwardemail.net"}
	s.IMAPTrustARCAuthservIDs = []string{"mx1.forwardemail.net", "mx2.forwardemail.net"}
	s.IMAPTrustARCRecipients = []string{"subscriber@example.org"}
	if _, err := buildIngester(s, CategoryConfig{}, store.NewMemoryStore(), o); err != nil {
		t.Fatalf("promo source with ARC trust refused: %v", err)
	}
	noRecip := s
	noRecip.IMAPTrustARCRecipients = nil
	if _, err := buildIngester(noRecip, CategoryConfig{}, store.NewMemoryStore(), o); err == nil {
		t.Fatal("ARC trust without recipients accepted")
	}
	// Other imap parsers may not trust ARC.
	plain := SourceConfig{
		Name: "lb", Category: "wine", Type: "imap", IMAPFrom: "offers@sender.example.com", IMAPParser: "whatever",
		WineChannel: "retailer", Origin: "US-CA", IMAPTrustARCSealers: s.IMAPTrustARCSealers,
	}
	if _, err := buildIngester(plain, CategoryConfig{WineShipTo: "US-WA"}, store.NewMemoryStore(), o); err == nil || !strings.Contains(err.Error(), "ARC trust") {
		t.Fatalf("plain imap source with ARC trust: %v", err)
	}
	half := s
	half.IMAPTrustARCAuthservIDs = nil
	if _, err := buildIngester(half, CategoryConfig{}, store.NewMemoryStore(), o); err == nil {
		t.Fatal("sealers without authserv-ids accepted")
	}
	// Deal sources read the same messages: their ARC trust must agree.
	srcs := dealSources()
	srcs[0].IMAPTrustARCSealers = []string{"forwardemail.net"}
	srcs[0].IMAPTrustARCAuthservIDs = []string{"mx1.forwardemail.net"}
	srcs[0].IMAPTrustARCRecipients = []string{"deals@example.net"}
	if _, err := buildDealHub(srcs, nil); err == nil || !strings.Contains(err.Error(), "ARC trust") {
		t.Fatalf("deal sources disagreeing on ARC trust: %v", err)
	}
	for i := range srcs {
		srcs[i].IMAPTrustARCSealers = []string{"forwardemail.net"}
		srcs[i].IMAPTrustARCAuthservIDs = []string{"mx1.forwardemail.net"}
		srcs[i].IMAPTrustARCRecipients = []string{"deals@example.net"}
	}
	if _, err := buildDealHub(srcs, nil); err != nil {
		t.Fatalf("agreeing deal sources refused: %v", err)
	}
}

func TestIMAPAuthMetrics(t *testing.T) {
	var b bytes.Buffer
	writeIMAPAuthMetrics(&b, []*pipeline.Ingester{{Connector: &fakeMail{id: "imap:promo-newegg"}}})
	for _, p := range []string{"dkim", "auth_results", "arc"} {
		if !strings.Contains(b.String(), `nagus_imap_authenticated_total{path="`+p+`"}`) {
			t.Errorf("missing path %s:\n%s", p, b.String())
		}
	}
	b.Reset()
	writeIMAPAuthMetrics(&b, []*pipeline.Ingester{{Connector: &fakeMail{id: "shopify:x"}}})
	if b.Len() != 0 {
		t.Fatal("imap auth metrics without an imap source")
	}
}
