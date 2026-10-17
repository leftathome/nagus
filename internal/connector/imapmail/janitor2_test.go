package imapmail

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// recordingSession records how the mailbox was opened.
type recordingSession struct {
	imapserver.SessionIMAP4rev2
	mu       *sync.Mutex
	readOnly *[]bool
}

func (s recordingSession) Select(name string, o *imap.SelectOptions) (*imap.SelectData, error) {
	s.mu.Lock()
	*s.readOnly = append(*s.readOnly, o != nil && o.ReadOnly)
	s.mu.Unlock()
	return s.SessionIMAP4rev2.Select(name, o)
}

type testMail struct {
	raw   string
	at    time.Time
	flags []imap.Flag
}

// janitorServer is an in-process IMAP server with the given capabilities;
// it records whether each SELECT was read-only.
func janitorServer(t *testing.T, caps imap.CapSet, mails []testMail) (string, func() []bool) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(janitorBox, "pw")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	var mu sync.Mutex
	var ro []bool
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return recordingSession{SessionIMAP4rev2: mem.NewSession().(imapserver.SessionIMAP4rev2), mu: &mu, readOnly: &ro}, nil, nil
		},
		Caps: caps, InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(janitorBox, "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	for _, m := range mails {
		cmd := c.Append("INBOX", int64(len(m.raw)), &imap.AppendOptions{Time: m.at, Flags: m.flags})
		if _, err := cmd.Write([]byte(m.raw)); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	ro = nil
	mu.Unlock()
	return ln.Addr().String(), func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return append([]bool(nil), ro...)
	}
}

var rev2 = imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}

// Without UID EXPUNGE the janitor deletes nothing: a plain EXPUNGE would
// also remove a message a human flagged \Deleted.
func TestJanitorNoUIDExpungeFailsClosed(t *testing.T) {
	addr, _ := janitorServer(t, imap.CapSet{imap.CapIMAP4rev1: {}}, []testMail{
		{raw: eml("old@x", senderA, "Old", ""), at: now.AddDate(0, 0, -40)},
		{raw: eml("human@x", senderA, "Human", ""), at: now.AddDate(0, 0, -1), flags: []imap.Flag{imap.FlagDeleted}},
	})
	j := janitorFor(t, addr, JanitorConfig{DeleteAfterDays: 30})
	if err := j.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	left := allMessages(t, addr)
	if len(left) != 2 || j.Stats().DeleteSkipped != 1 || j.Stats().Deleted != 0 {
		t.Fatalf("left %d, stats %+v", len(left), j.Stats())
	}
	if hasFlag(left["old@x"], imap.FlagDeleted) {
		t.Fatal("the old message was flagged although nothing can be expunged safely")
	}
	// With UID EXPUNGE, the human-flagged young message survives.
	addr2, _ := janitorServer(t, rev2, []testMail{
		{raw: eml("old@x", senderA, "Old", ""), at: now.AddDate(0, 0, -40)},
		{raw: eml("human@x", senderA, "Human", ""), at: now.AddDate(0, 0, -1), flags: []imap.Flag{imap.FlagDeleted}},
	})
	j2 := janitorFor(t, addr2, JanitorConfig{DeleteAfterDays: 30})
	if err := j2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	left = allMessages(t, addr2)
	if _, ok := left["human@x"]; !ok || len(left) != 1 {
		t.Fatalf("UID EXPUNGE removed someone else's message: %v", left)
	}
}

// Age is measured exactly from INTERNALDATE, whatever its time zone.
func TestJanitorDeleteBoundaryAcrossZones(t *testing.T) {
	west := time.FixedZone("-1200", -12*3600)
	east := time.FixedZone("+1400", 14*3600)
	day := 24 * time.Hour
	addr, _ := janitorServer(t, rev2, []testMail{
		{raw: eml("young-west@x", senderA, "a", ""), at: now.Add(-30*day + 30*time.Minute).In(west)},
		{raw: eml("young-east@x", senderA, "b", ""), at: now.Add(-30*day + time.Hour).In(east)},
		{raw: eml("old-west@x", senderA, "c", ""), at: now.Add(-30*day - time.Minute).In(west)},
		{raw: eml("old-east@x", senderA, "d", ""), at: now.Add(-30*day - time.Hour).In(east)},
	})
	j := janitorFor(t, addr, JanitorConfig{DeleteAfterDays: 30})
	if err := j.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	left := allMessages(t, addr)
	for _, id := range []string{"young-west@x", "young-east@x"} {
		if _, ok := left[id]; !ok {
			t.Errorf("%s (younger than 30 days) was deleted", id)
		}
	}
	for _, id := range []string{"old-west@x", "old-east@x"} {
		if _, ok := left[id]; ok {
			t.Errorf("%s (older than 30 days) was kept", id)
		}
	}
}

// A message that arrived within the clock margin before a fetch began is
// not yet credited to it; one before the margin is.
func TestJanitorSkewWindow(t *testing.T) {
	started := now
	addr, _ := janitorServer(t, rev2, []testMail{
		{raw: eml("inside@x", senderA, "a", ""), at: started.Add(-time.Minute)},
		{raw: eml("outside@x", senderA, "b", ""), at: started.Add(-3 * time.Minute)},
	})
	uv := uidValidity(t, addr)
	r := &fakeReport{r: FetchReport{Started: started, Complete: true, UIDValidity: uv}}
	j := janitorFor(t, addr, JanitorConfig{MarkSeen: true, Sources: []JanitorSource{{Name: "a", Addresses: []string{senderA}, Report: r.get}}})
	_ = j.Run(context.Background())
	left := allMessages(t, addr)
	if hasFlag(left["inside@x"], imap.FlagSeen) || !hasFlag(left["outside@x"], imap.FlagSeen) {
		t.Fatalf("skew window: inside %v outside %v", left["inside@x"], left["outside@x"])
	}
}

// A dry run opens the mailbox read-only (EXAMINE); a live run read-write.
func TestJanitorDryRunUsesExamine(t *testing.T) {
	addr, opened := janitorServer(t, rev2, []testMail{{raw: eml("old@x", senderA, "Old", ""), at: now.AddDate(0, 0, -40)}})
	dry := janitorFor(t, addr, JanitorConfig{DeleteAfterDays: 30, DryRun: true})
	_ = dry.Run(context.Background())
	if o := opened(); len(o) == 0 || !o[0] {
		t.Fatalf("dry run selects: %v", o)
	}
	live := janitorFor(t, addr, JanitorConfig{DeleteAfterDays: 30})
	_ = live.Run(context.Background())
	if o := opened(); len(o) < 2 || o[len(o)-1] {
		t.Fatalf("live run selects: %v", o)
	}
}

// A cancelled context ends a pass promptly.
func TestJanitorHonoursContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() { // accepts and never speaks
		c, err := ln.Accept()
		if err == nil {
			defer func() { _ = c.Close() }()
			_, _ = io.Copy(io.Discard, c) // reads until the client hangs up
		}
	}()
	j := janitorFor(t, ln.Addr().String(), JanitorConfig{MarkSeen: true})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- j.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a pass against a silent server succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pass did not end when its context was cancelled")
	}
}
