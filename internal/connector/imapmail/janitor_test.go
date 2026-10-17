package imapmail

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	janitorBox = "deals@totally.apocryph.al"
	senderA    = "a@sender-a.example.com"
	senderB    = "b@sender-b.example.com"
)

func allMessages(t *testing.T, addr string) map[string][]imap.Flag {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(janitorBox, "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	sel, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]imap.Flag{}
	if sel.NumMessages == 0 {
		return out
	}
	var set imap.SeqSet
	set.AddRange(1, sel.NumMessages)
	msgs, err := c.Fetch(set, &imap.FetchOptions{Envelope: true, Flags: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		out[m.Envelope.MessageID] = m.Flags
	}
	return out
}

func uidValidity(t *testing.T, addr string) uint32 {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.Login(janitorBox, "pw").Wait()
	sel, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	return sel.UIDValidity
}

func hasFlag(fs []imap.Flag, f imap.Flag) bool {
	for _, x := range fs {
		if strings.EqualFold(string(x), string(f)) {
			return true
		}
	}
	return false
}

type fakeReport struct{ r FetchReport }

func (f *fakeReport) get() FetchReport { return f.r }

func janitorFor(t *testing.T, addr string, cfg JanitorConfig) *Janitor {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	cfg.Host, cfg.Port, cfg.TLS, cfg.Username, cfg.Password = host, port, "none", janitorBox, "pw"
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return now }
	}
	cfg.Logf = t.Logf
	j, err := NewJanitor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestJanitorMarksOnlyWhatEverySourceRead(t *testing.T) {
	arrived := now.Add(-time.Hour)
	addr := server(t, map[string]time.Time{
		eml("a1@x", senderA, "A", ""):             arrived,
		eml("b1@x", senderB, "B", ""):             arrived,
		eml("u1@x", "u@unknown.example", "U", ""): arrived,
	})
	uv := uidValidity(t, addr)
	ra, rb := &fakeReport{}, &fakeReport{}
	j := janitorFor(t, addr, JanitorConfig{MarkSeen: true, Sources: []JanitorSource{
		{Name: "a", Addresses: []string{senderA}, Report: ra.get},
		{Name: "b", Addresses: []string{senderB}, Report: rb.get},
	}})
	ctx := context.Background()
	marked := func() map[string]bool {
		out := map[string]bool{}
		for id, fs := range allMessages(t, addr) {
			out[id] = hasFlag(fs, imap.FlagSeen)
			if out[id] && !hasFlag(fs, DefaultKeyword) {
				t.Errorf("%s: Seen without the keyword", id)
			}
		}
		return out
	}
	// No source has read anything: nothing is marked.
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if m := marked(); m["a1@x"] || m["b1@x"] || m["u1@x"] {
		t.Fatalf("marked before any fetch: %v", m)
	}
	// Source a read after arrival: its mail is marked; b's and the unknown
	// sender's (which needs every source) are not.
	ra.r = FetchReport{Started: now, Complete: true, UIDValidity: uv, Pending: map[uint32]bool{}}
	rb.r = FetchReport{Started: arrived.Add(-time.Minute), Complete: true, UIDValidity: uv}
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if m := marked(); !m["a1@x"] || m["b1@x"] || m["u1@x"] {
		t.Fatalf("after a: %v", m)
	}
	// b read but its fetch was incomplete: still not.
	rb.r = FetchReport{Started: now, Complete: false, UIDValidity: uv}
	_ = j.Run(ctx)
	if m := marked(); m["b1@x"] {
		t.Fatal("an incomplete fetch marked mail")
	}
	// b complete under another UIDVALIDITY: still not.
	rb.r = FetchReport{Started: now, Complete: true, UIDValidity: uv + 1}
	_ = j.Run(ctx)
	if m := marked(); m["b1@x"] {
		t.Fatal("a fetch of another UIDVALIDITY marked mail")
	}
	// b complete: b's mail and the unknown sender's are marked.
	rb.r = FetchReport{Started: now, Complete: true, UIDValidity: uv}
	_ = j.Run(ctx)
	if m := marked(); !m["b1@x"] || !m["u1@x"] {
		t.Fatalf("after b: %v", m)
	}
	if st := j.Stats(); st.Marked != 3 || st.Runs != 5 || st.Errors != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestJanitorPendingAndDryRun(t *testing.T) {
	addr := server(t, map[string]time.Time{eml("a1@x", senderA, "A", ""): now.Add(-time.Hour)})
	uv := uidValidity(t, addr)
	// The one message is UID 1: pending (a temporary refusal) keeps it.
	r := &fakeReport{r: FetchReport{Started: now, Complete: true, UIDValidity: uv, Pending: map[uint32]bool{1: true}}}
	src := []JanitorSource{{Name: "a", Addresses: []string{senderA}, Report: r.get}}
	j := janitorFor(t, addr, JanitorConfig{MarkSeen: true, Sources: src})
	_ = j.Run(context.Background())
	if hasFlag(allMessages(t, addr)["a1@x"], imap.FlagSeen) {
		t.Fatal("a pending message was marked")
	}
	r.r.Pending = nil
	dry := janitorFor(t, addr, JanitorConfig{MarkSeen: true, DryRun: true, Sources: src})
	_ = dry.Run(context.Background())
	if hasFlag(allMessages(t, addr)["a1@x"], imap.FlagSeen) || dry.Stats().WouldMark != 1 {
		t.Fatalf("dry run changed the mailbox or did not count (%+v)", dry.Stats())
	}
}

func TestJanitorDeletesOldMailOncePerDay(t *testing.T) {
	addr := server(t, map[string]time.Time{
		eml("old@x", senderA, "Old", ""):    now.AddDate(0, 0, -40),
		eml("recent@x", senderA, "New", ""): now.AddDate(0, 0, -20),
	})
	clock := now
	dry := janitorFor(t, addr, JanitorConfig{DeleteAfterDays: 30, DryRun: true, Now: func() time.Time { return clock }})
	_ = dry.Run(context.Background())
	if len(allMessages(t, addr)) != 2 || dry.Stats().WouldDelete != 1 {
		t.Fatalf("dry run deleted or did not count: %+v", dry.Stats())
	}
	j := janitorFor(t, addr, JanitorConfig{DeleteAfterDays: 30, Now: func() time.Time { return clock }})
	if err := j.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	left := allMessages(t, addr)
	if _, ok := left["old@x"]; ok || len(left) != 1 {
		t.Fatalf("after delete: %v", left)
	}
	// Twelve days on, the recent one is old too -- but not before a day
	// has passed since the last delete run.
	clock = now.Add(time.Hour)
	_ = j.Run(context.Background())
	if len(allMessages(t, addr)) != 1 {
		t.Fatal("deleted twice in a day")
	}
	clock = now.AddDate(0, 0, 12)
	_ = j.Run(context.Background())
	if len(allMessages(t, addr)) != 0 || j.Stats().Deleted != 2 {
		t.Fatalf("second day: %d left, stats %+v", len(allMessages(t, addr)), j.Stats())
	}
}

func TestJanitorConfig(t *testing.T) {
	for name, cfg := range map[string]JanitorConfig{
		"nothing to do": {Host: "h", Username: "u", Password: "p"},
		"no host":       {Username: "u", Password: "p", MarkSeen: true},
		"bad keyword":   {Host: "h", Username: "u", Password: "p", MarkSeen: true, Keyword: "Nagus Processed"},
		"negative days": {Host: "h", Username: "u", Password: "p", DeleteAfterDays: -1, MarkSeen: true},
	} {
		if _, err := NewJanitor(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
