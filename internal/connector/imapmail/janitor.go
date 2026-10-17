package imapmail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// The mailbox janitor (nagus-c0s; operator decision 2026-10-17: "mark read
// now, delete after 30d"). Every nagus imap source reads ONE shared mailbox
// read-only (EXAMINE) and filters it by sender; the janitor is the only
// writer. It is configured once per mailbox:
//
//   - MARK: a message gets \Seen and the keyword (default $NagusProcessed)
//     once every source whose senders match its From has completed a fetch
//     that BEGAN after the message arrived (INTERNALDATE) and did not leave
//     it pending (a temporary DKIM key lookup failure); a message matching no
//     source is marked once EVERY source has done so. A source re-reads its
//     whole lookback window on each poll, so such a fetch has processed, or
//     permanently refused, every message of its senders that was there. No
//     per-source keyword or table: the sources' last fetch reports, held in
//     memory, are the record; after a restart marking simply waits one poll
//     cycle. Marking changes nothing nagus reads (the sources search by date
//     and sender, never by \Seen).
//   - DELETE: at most once a day, every message whose INTERNALDATE is at
//     least DeleteAfterDays * 24h old (measured exactly: SEARCH BEFORE only
//     narrows candidates, see delete) gets \Deleted and is removed with
//     UID EXPUNGE. A server without UID EXPUNGE (no UIDPLUS, not IMAP4rev2)
//     is NOT cleaned: a plain EXPUNGE would also remove messages a human or
//     another client flagged \Deleted. The caller refuses a DeleteAfterDays
//     that is not beyond every sharing source's lookback plus a day, so
//     nothing a source still reads is deleted.
//
// DryRun counts what would be marked and deleted and changes nothing. Logs
// carry counts only, never an address.

// DefaultKeyword marks a message nagus has processed.
const DefaultKeyword = "$NagusProcessed"

// janitorSkew is the clock margin between a message's INTERNALDATE (the
// server's clock) and a fetch's start (ours).
const janitorSkew = 2 * time.Minute

// maxMarkBatch bounds the messages examined per run.
const maxMarkBatch = 2000

// JanitorSource is one source sharing the mailbox.
type JanitorSource struct {
	Name string
	// Addresses are the source's senders and forwarders, lower-cased.
	Addresses []string
	// Report is the source's last completed fetch, with Complete false when
	// the source (or its wrapper) reported that fetch incomplete.
	Report func() FetchReport
}

// JanitorConfig configures the janitor of one mailbox.
type JanitorConfig struct {
	Host, Port, Username, Password, TLS string
	Mailbox                             string
	MarkSeen                            bool
	Keyword                             string
	// DeleteAfterDays deletes messages older than this many days; 0 never
	// deletes.
	DeleteAfterDays int
	DryRun          bool
	Sources         []JanitorSource
	Now             func() time.Time
	Logf            func(string, ...any)
}

// JanitorStats are the janitor's counters.
type JanitorStats struct {
	Runs, Errors         int64
	Marked, WouldMark    int64
	Deleted, WouldDelete int64
	// DeleteSkipped counts delete passes skipped because the server has no
	// UID EXPUNGE.
	DeleteSkipped          int64
	LastRun, LastDeleteRun time.Time
}

// runTimeout bounds one janitor pass on the wire.
const runTimeout = 5 * time.Minute

// Janitor is the single writer of a shared mailbox.
type Janitor struct {
	cfg JanitorConfig

	mu         sync.Mutex
	stats      JanitorStats
	lastDelete time.Time
}

// NewJanitor validates the configuration.
func NewJanitor(cfg JanitorConfig) (*Janitor, error) {
	if cfg.Host == "" || cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("imapmail: janitor: host, username and password are required")
	}
	if !cfg.MarkSeen && cfg.DeleteAfterDays <= 0 {
		return nil, errors.New("imapmail: janitor: nothing to do (markSeen off, deleteAfterDays 0)")
	}
	if cfg.DeleteAfterDays < 0 {
		return nil, errors.New("imapmail: janitor: deleteAfterDays must not be negative")
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	if cfg.TLS == "" {
		cfg.TLS = "implicit"
	}
	if cfg.Port == "" {
		cfg.Port = "993"
		if cfg.TLS == "none" {
			cfg.Port = "143"
		}
	}
	if cfg.Keyword == "" {
		cfg.Keyword = DefaultKeyword
	}
	if !strings.HasPrefix(cfg.Keyword, "$") || strings.ContainsAny(cfg.Keyword, " ()[]{}%*\"\\") {
		return nil, fmt.Errorf("imapmail: janitor: keyword %q (want an IMAP keyword such as $NagusProcessed)", cfg.Keyword)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Janitor{cfg: cfg}, nil
}

// Stats snapshots the counters.
func (j *Janitor) Stats() JanitorStats {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stats
}

func (j *Janitor) logf(f string, a ...any) {
	if j.cfg.Logf != nil {
		j.cfg.Logf(f, a...)
	}
}

// dial connects with ctx and a deadline on the whole pass, so a stalled
// server cannot hang the janitor's goroutine.
func (j *Janitor) dial(ctx context.Context) (*imapclient.Client, net.Conn, error) {
	addr := net.JoinHostPort(j.cfg.Host, j.cfg.Port)
	d := net.Dialer{Timeout: 30 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	_ = raw.SetDeadline(time.Now().Add(runTimeout))
	tlsCfg := &tls.Config{ServerName: j.cfg.Host, MinVersion: tls.VersionTLS12}
	switch j.cfg.TLS {
	case "implicit":
		tc := tls.Client(raw, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, nil, err
		}
		return imapclient.New(tc, &imapclient.Options{TLSConfig: tlsCfg}), raw, nil
	case "starttls":
		cl, err := imapclient.NewStartTLS(raw, &imapclient.Options{TLSConfig: tlsCfg})
		if err != nil {
			_ = raw.Close()
			return nil, nil, err
		}
		return cl, raw, nil
	case "none":
		return imapclient.New(raw, nil), raw, nil
	default:
		_ = raw.Close()
		return nil, nil, fmt.Errorf("tls mode %q (want implicit, starttls or none)", j.cfg.TLS)
	}
}

// Run makes one pass: mark, then (at most once a day) delete.
func (j *Janitor) Run(ctx context.Context) error {
	err := j.run(ctx)
	j.mu.Lock()
	j.stats.Runs++
	j.stats.LastRun = j.cfg.Now()
	if err != nil {
		j.stats.Errors++
	}
	j.mu.Unlock()
	return err
}

func (j *Janitor) run(ctx context.Context) error {
	cl, raw, err := j.dial(ctx)
	if err != nil {
		return fmt.Errorf("imapmail janitor: %w", err)
	}
	defer func() { _ = cl.Close() }()
	// A cancelled context closes the connection, ending any blocked command.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-stop:
		}
	}()
	if err := cl.Login(j.cfg.Username, j.cfg.Password).Wait(); err != nil {
		return fmt.Errorf("imapmail janitor: login: %w", err)
	}
	// SELECT, read-write: the janitor is the mailbox's only writer. A dry
	// run changes nothing, not even \Recent: EXAMINE.
	sel, err := cl.Select(j.cfg.Mailbox, &imap.SelectOptions{ReadOnly: j.cfg.DryRun}).Wait()
	if err != nil {
		return fmt.Errorf("imapmail janitor: select %q: %w", j.cfg.Mailbox, err)
	}
	if j.cfg.MarkSeen {
		if err := j.mark(cl, sel); err != nil {
			return err
		}
	}
	if j.cfg.DeleteAfterDays > 0 {
		now := j.cfg.Now()
		j.mu.Lock()
		due := now.Sub(j.lastDelete) >= 24*time.Hour
		j.mu.Unlock()
		if due {
			if err := j.delete(cl, now); err != nil {
				return err
			}
			j.mu.Lock()
			j.lastDelete, j.stats.LastDeleteRun = now, now
			j.mu.Unlock()
		}
	}
	return nil
}

// covered reports whether every relevant source has read a message.
func (j *Janitor) covered(from string, uid uint32, arrived time.Time, uidValidity uint32) bool {
	var relevant []JanitorSource
	for _, s := range j.cfg.Sources {
		if from != "" && slices.Contains(s.Addresses, from) {
			relevant = append(relevant, s)
		}
	}
	if len(relevant) == 0 {
		relevant = j.cfg.Sources // no source reads it: one full cycle of all
	}
	if len(relevant) == 0 {
		return false
	}
	for _, s := range relevant {
		r := s.Report()
		if !r.Complete || r.UIDValidity != uidValidity || !r.Started.After(arrived.Add(janitorSkew)) || r.Pending[uid] {
			return false
		}
	}
	return true
}

func (j *Janitor) mark(cl *imapclient.Client, sel *imap.SelectData) error {
	kw := imap.Flag(j.cfg.Keyword)
	keywords := slices.Contains(sel.PermanentFlags, imap.FlagWildcard) || slices.Contains(sel.PermanentFlags, kw)
	criteria := &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}
	if keywords {
		// Humans may have read a message; the keyword is nagus's own mark.
		criteria = &imap.SearchCriteria{NotFlag: []imap.Flag{kw}}
	}
	found, err := cl.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return fmt.Errorf("imapmail janitor: search: %w", err)
	}
	uids := found.AllUIDs()
	if len(uids) == 0 {
		return nil
	}
	if len(uids) > maxMarkBatch {
		uids = uids[len(uids)-maxMarkBatch:]
	}
	msgs, err := cl.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, Envelope: true, InternalDate: true}).Collect()
	if err != nil {
		return fmt.Errorf("imapmail janitor: fetch envelopes: %w", err)
	}
	var done []imap.UID
	for _, m := range msgs {
		from := ""
		if m.Envelope != nil && len(m.Envelope.From) == 1 {
			from = strings.ToLower(m.Envelope.From[0].Addr())
		}
		if j.covered(from, uint32(m.UID), m.InternalDate, sel.UIDValidity) {
			done = append(done, m.UID)
		}
	}
	if len(done) == 0 {
		return nil
	}
	if j.cfg.DryRun {
		j.mu.Lock()
		j.stats.WouldMark += int64(len(done))
		j.mu.Unlock()
		j.logf("imapmail janitor %s: DRY RUN: would mark %d of %d unmarked message(s) processed", j.cfg.Mailbox, len(done), len(msgs))
		return nil
	}
	flags := []imap.Flag{imap.FlagSeen}
	if keywords {
		flags = append(flags, kw)
	}
	if err := cl.Store(imap.UIDSetNum(done...), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: flags}, nil).Close(); err != nil {
		return fmt.Errorf("imapmail janitor: store flags: %w", err)
	}
	j.mu.Lock()
	j.stats.Marked += int64(len(done))
	j.mu.Unlock()
	j.logf("imapmail janitor %s: marked %d of %d unmarked message(s) processed", j.cfg.Mailbox, len(done), len(msgs))
	return nil
}

func (j *Janitor) delete(cl *imapclient.Client, now time.Time) error {
	if !cl.Caps().Has(imap.CapUIDPlus) && !cl.Caps().Has(imap.CapIMAP4rev2) {
		// Fail closed: a plain EXPUNGE removes EVERY \Deleted message,
		// including ones a human or another client flagged.
		j.mu.Lock()
		j.stats.DeleteSkipped++
		j.mu.Unlock()
		j.logf("imapmail janitor %s: the server has no UID EXPUNGE (UIDPLUS); old mail is NOT deleted", j.cfg.Mailbox)
		return nil
	}
	maxAge := time.Duration(j.cfg.DeleteAfterDays) * 24 * time.Hour
	// SEARCH BEFORE compares DATES in the server's view of each
	// INTERNALDATE's zone, which may differ from ours by up to a day either
	// way. It only narrows the candidates -- a day LATER than the cutoff, so
	// none is missed -- and each candidate's exact INTERNALDATE decides.
	found, err := cl.UIDSearch(&imap.SearchCriteria{Before: now.UTC().Add(-maxAge).AddDate(0, 0, 2)}, nil).Wait()
	if err != nil {
		return fmt.Errorf("imapmail janitor: search old: %w", err)
	}
	cands := found.AllUIDs()
	if len(cands) == 0 {
		return nil
	}
	msgs, err := cl.Fetch(imap.UIDSetNum(cands...), &imap.FetchOptions{UID: true, InternalDate: true}).Collect()
	if err != nil {
		return fmt.Errorf("imapmail janitor: fetch dates: %w", err)
	}
	var uids []imap.UID
	for _, m := range msgs {
		if !m.InternalDate.IsZero() && now.Sub(m.InternalDate) >= maxAge {
			uids = append(uids, m.UID)
		}
	}
	if len(uids) == 0 {
		return nil
	}
	if j.cfg.DryRun {
		j.mu.Lock()
		j.stats.WouldDelete += int64(len(uids))
		j.mu.Unlock()
		j.logf("imapmail janitor %s: DRY RUN: would delete %d message(s) older than %d days", j.cfg.Mailbox, len(uids), j.cfg.DeleteAfterDays)
		return nil
	}
	set := imap.UIDSetNum(uids...)
	if err := cl.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		return fmt.Errorf("imapmail janitor: flag deleted: %w", err)
	}
	if err := cl.UIDExpunge(set).Close(); err != nil {
		return fmt.Errorf("imapmail janitor: expunge: %w", err)
	}
	j.mu.Lock()
	j.stats.Deleted += int64(len(uids))
	j.mu.Unlock()
	j.logf("imapmail janitor %s: deleted %d message(s) older than %d days", j.cfg.Mailbox, len(uids), j.cfg.DeleteAfterDays)
	return nil
}
