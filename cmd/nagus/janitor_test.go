package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/promo"
	"github.com/leftathome/nagus/internal/store"
)

type nopParser struct{}

func (nopParser) Parse(imapmail.Message) ([]listing.Raw, error) { return nil, nil }

// incompleteConn wraps a connector and reports its fetch incomplete.
type incompleteConn struct{ listing.Connector }

func (incompleteConn) FetchComplete() bool { return false }

func janitorFixture(t *testing.T, lookback int) (map[string]imapConn, []*pipeline.Ingester) {
	t.Helper()
	t.Setenv("NAGUS_IMAP_HOST", "imap.example.net")
	t.Setenv("NAGUS_IMAP_USERNAME", "deals@example.net")
	t.Setenv("NAGUS_IMAP_PASSWORD", "x")
	conns := map[string]imapConn{}
	var ings []*pipeline.Ingester
	for _, name := range []string{"a", "b"} {
		c, err := imapmail.NewConnector(imapmail.Config{Name: name, Host: "imap.example.net", Username: "u", Password: "p",
			Senders: []string{name + "@sender.example.com"}, Parser: nopParser{}, LookbackDays: lookback})
		if err != nil {
			t.Fatal(err)
		}
		conns[c.SourceID()] = imapConn{conn: c}
		var outer listing.Connector = c
		if name == "b" {
			outer = incompleteConn{c}
		}
		ings = append(ings, &pipeline.Ingester{Connector: outer})
	}
	return conns, ings
}

func TestBuildJanitor(t *testing.T) {
	conns, ings := janitorFixture(t, 14)
	if j, err := buildJanitor(nil, conns, ings, t.Logf); j != nil || err != nil {
		t.Fatal("an absent janitor was built")
	}
	if j, err := buildJanitor(&IMAPJanitorConfig{MarkSeen: true, DeleteAfterDays: 30}, conns, ings, t.Logf); j != nil || err != nil {
		t.Fatal("a disabled janitor was built")
	}
	for name, jc := range map[string]IMAPJanitorConfig{
		"delete within lookback": {Enabled: true, DeleteAfterDays: 14},
		"delete at lookback + 1": {Enabled: true, DeleteAfterDays: 15},
		"no source on mailbox":   {Enabled: true, MarkSeen: true, Mailbox: "Archive"},
		"nothing to do":          {Enabled: true},
		"negative delete":        {Enabled: true, MarkSeen: true, DeleteAfterDays: -1},
		"negative interval":      {Enabled: true, MarkSeen: true, IntervalMinutes: -1},
		"bad keyword":            {Enabled: true, MarkSeen: true, Keyword: "no dollar"},
	} {
		if _, err := buildJanitor(&jc, conns, ings, t.Logf); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	j, err := buildJanitor(&IMAPJanitorConfig{Enabled: true, MarkSeen: true, DeleteAfterDays: 16}, conns, ings, t.Logf)
	if err != nil || j == nil {
		t.Fatalf("a valid janitor was refused: %v", err)
	}
	var b bytes.Buffer
	writeJanitorMetrics(&b, j)
	for _, want := range []string{"nagus_imap_janitor_runs_total 0", "nagus_imap_janitor_deleted_total 0", "nagus_imap_janitor_last_run_timestamp_seconds 0"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

// Sources built through buildIngester register for the janitor, with their
// outer (wrapper) completeness.
func TestIMAPSourcesRegisterForTheJanitor(t *testing.T) {
	t.Setenv("NAGUS_IMAP_HOST", "imap.example.net")
	t.Setenv("NAGUS_IMAP_USERNAME", "deals@example.net")
	t.Setenv("NAGUS_IMAP_PASSWORD", "x")
	conns := map[string]imapConn{}
	o := categoryOpts{logf: t.Logf, promo: promoRuntimeFromEnv(nil), imapConns: &conns}
	s := promoSource("promo-newegg", "newegg", "hdd", "placeholder@newegg.com")
	ing, err := buildIngester(s, CategoryConfig{}, store.NewMemoryStore(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := conns["imap:promo-newegg"]; !ok {
		t.Fatalf("not registered: %v", conns)
	}
	if _, ok := ing.Connector.(*promo.Source); !ok || ing.MailStateWindow != promoStateWindow(s) {
		t.Fatalf("ingester %T window %s", ing.Connector, ing.MailStateWindow)
	}
}

// dryRun defaults to true; only an explicit false writes.
func TestJanitorDryRunDefault(t *testing.T) {
	f := false
	for _, tc := range []struct {
		jc   IMAPJanitorConfig
		want bool
	}{
		{IMAPJanitorConfig{}, true},
		{IMAPJanitorConfig{DryRun: &f}, false},
	} {
		if got := tc.jc.dryRun(); got != tc.want {
			t.Errorf("dryRun(%+v) = %v", tc.jc, got)
		}
	}
	var jc IMAPJanitorConfig
	if err := json.Unmarshal([]byte(`{"enabled":true,"deleteAfterDays":30}`), &jc); err != nil || !jc.dryRun() {
		t.Fatalf("a config without dryRun is live: %v", err)
	}
}

func TestSameMailbox(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"INBOX", "inbox", true}, {"Inbox", "INBOX", true}, {"Deals", "Deals", true}, {"Deals", "deals", false}, {"INBOX", "Deals", false},
	} {
		if got := sameMailbox(tc.a, tc.b); got != tc.want {
			t.Errorf("sameMailbox(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}
