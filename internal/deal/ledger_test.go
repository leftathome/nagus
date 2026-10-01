package deal

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/sanitize"
)

func testHub() *Hub {
	return NewHub(map[string]string{"wine": "imap:deals-wine", "hdd": "imap:deals-hdd"},
		map[string]string{"Agent@Example.org": "Caspar"}, "deals@example.net", 14, nil)
}

func msg(id, text string) imapmail.Message {
	return imapmail.Message{ID: id, From: "agent@example.org", Date: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), Text: text}
}

// fetch runs one source's parse as its connector would, returning its raws.
func fetch(t *testing.T, h *Hub, cat string, m imapmail.Message) []string {
	t.Helper()
	p, err := NewParser(h, cat)
	if err != nil {
		t.Fatal(err)
	}
	h.Ledger.startFetch(h.Sources[cat])
	raws, err := p.Parse(m)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range raws {
		keys = append(keys, r.SourceKey)
	}
	return keys
}

// A gate outage leaves the line pending with a reason, and the next poll
// accepts it; the line is counted once, at its final outcome.
func TestTransientGateFailureIsRetried(t *testing.T) {
	h := testHub()
	m := msg("m1@example.org", ExampleHDD+"\n")
	keys := fetch(t, h, "hdd", m)
	down := fmt.Errorf("sanitize: glovebox unreachable, dropping (fail closed): %w", errors.New("dial tcp: refused"))
	h.Ledger.ApplyIngest("imap:deals-hdd", pipeline.IngestResult{Skips: []pipeline.Skip{{SourceKey: keys[0], Stage: "sanitize", Err: down}}})
	v, _ := h.Ledger.Lookup("m1@example.org")
	if v.Outcome != "pending" || v.Lines[0].Reason != string(ReasonGateUnavailable) {
		t.Fatalf("outage: %+v", v)
	}
	fetch(t, h, "hdd", m)
	h.Ledger.ApplyIngest("imap:deals-hdd", pipeline.IngestResult{})
	v, _ = h.Ledger.Lookup("m1@example.org")
	if v.Outcome != MsgAccepted || v.Lines[0].OfferID == "" {
		t.Fatalf("after recovery: %+v", v)
	}
	var b strings.Builder
	h.Ledger.WriteMetrics(&b)
	if !strings.Contains(b.String(), `nagus_deal_submissions_lines_total{outcome="accepted",reason="none"} 1`+"\n") ||
		!strings.Contains(b.String(), `nagus_deal_submissions_total{outcome="accepted"} 1`+"\n") {
		t.Fatal(b.String())
	}
}

func TestQuarantineIsFinal(t *testing.T) {
	h := testHub()
	keys := fetch(t, h, "hdd", msg("m2@example.org", ExampleHDD+"\n"))
	q := fmt.Errorf("%w: score 0.9", sanitize.ErrQuarantined)
	h.Ledger.ApplyIngest("imap:deals-hdd", pipeline.IngestResult{Skips: []pipeline.Skip{{SourceKey: keys[0], Stage: "sanitize", Err: q}}})
	if v, _ := h.Ledger.Lookup("m2@example.org"); v.Outcome != MsgRejected || v.Lines[0].Reason != string(ReasonGateRefused) {
		t.Fatalf("%+v", v)
	}
}

// A message with lines for two categories is final only once BOTH deal
// sources have read it.
func TestMessageWaitsForEverySource(t *testing.T) {
	h := testHub()
	m := msg("m3@example.org", ExampleWine+"\n"+ExampleHDD+"\n")
	fetch(t, h, "wine", m)
	h.Ledger.ApplyIngest("imap:deals-wine", pipeline.IngestResult{})
	if v, _ := h.Ledger.Lookup("m3@example.org"); v.Outcome != "pending" || v.Counts.Accepted != 1 || v.Counts.Pending != 1 {
		t.Fatalf("after wine only: %+v", v)
	}
	fetch(t, h, "hdd", m)
	h.Ledger.ApplyIngest("imap:deals-hdd", pipeline.IngestResult{})
	if v, _ := h.Ledger.Lookup("m3@example.org"); v.Outcome != MsgAccepted || v.Counts.Accepted != 2 {
		t.Fatalf("after both: %+v", v)
	}
}

func TestCategoryNotEnabledAndLimits(t *testing.T) {
	h := NewHub(map[string]string{"hdd": "imap:deals-hdd"}, nil, "", 14, nil)
	var lines []string
	lines = append(lines, ExampleWine)
	for i := 0; i < MaxLinesPerMessage; i++ {
		lines = append(lines, ExampleHDD)
	}
	keys := fetch(t, h, "hdd", msg("m4@example.org", strings.Join(lines, "\n")))
	if len(keys) != MaxLinesPerMessage-1 {
		t.Fatalf("ingested %d lines, want the first %d deal lines minus the wine one", len(keys), MaxLinesPerMessage)
	}
	h.Ledger.ApplyIngest("imap:deals-hdd", pipeline.IngestResult{})
	v, _ := h.Ledger.Lookup("m4@example.org")
	if v.Lines[0].Reason != string(ReasonCategoryNotEnabled) || v.Lines[len(v.Lines)-1].Reason != string(ReasonTooManyLines) {
		t.Fatalf("first %+v last %+v", v.Lines[0], v.Lines[len(v.Lines)-1])
	}
}

func TestPrincipalAliasAndEmptyMessages(t *testing.T) {
	h := testHub()
	if h.Principal("AGENT@example.org") != "caspar" || h.Principal("other@example.org") != "other@example.org" {
		t.Fatal("principal mapping")
	}
	fetch(t, h, "hdd", msg("m5@example.org", "just a note, no deals\n"))
	fetch(t, h, "hdd", imapmail.Message{ID: "m6@example.org", From: "agent@example.org", HTML: "<p>deal</p>"})
	if v, _ := h.Ledger.Lookup("<m5@example.org>"); v.Outcome != MsgEmpty {
		t.Fatalf("%+v", v)
	}
	if v, _ := h.Ledger.Lookup("m6@example.org"); v.Outcome != MsgNoTextPart {
		t.Fatalf("%+v", v)
	}
	if got := h.Recent("caspar", 1); len(got) != 1 {
		t.Fatalf("limit: %d", len(got))
	}
}

// Messages leave the ledger once no poll has seen them for the retention.
func TestLedgerPrunes(t *testing.T) {
	now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	h := NewHub(map[string]string{"hdd": "imap:deals-hdd"}, nil, "", 14, func() time.Time { return now })
	fetch(t, h, "hdd", msg("old@example.org", ExampleHDD+"\n"))
	now = now.Add(17 * 24 * time.Hour)
	h.Ledger.ApplyIngest("imap:deals-hdd", pipeline.IngestResult{})
	if _, ok := h.Ledger.Lookup("old@example.org"); ok {
		t.Fatal("not pruned")
	}
}

// A reply quoting an earlier submission with no > prefix, and a signature
// holding JSON, add no lines.
func TestBoundariesStopReading(t *testing.T) {
	h := testHub()
	for _, tail := range []string{
		"-- \n" + ExampleHDD,
		"On Sat, Sep 26, 2026 at 9:00 AM Someone <someone@example.org> wrote:\n" + ExampleHDD,
		"-----Original Message-----\n" + ExampleHDD,
		"> " + ExampleHDD,
	} {
		keys := fetch(t, h, "hdd", msg("b-"+fmt.Sprint(len(tail))+"@example.org", ExampleHDD+"\n\n"+tail+"\n"))
		if len(keys) != 1 {
			t.Errorf("tail %q: %d lines read, want 1", tail[:12], len(keys))
		}
	}
}
