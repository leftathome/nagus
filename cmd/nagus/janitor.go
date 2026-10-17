package main

// The deals-mailbox janitor (nagus-c0s; operator decision 2026-10-17: "mark
// read now, delete after 30d"). Configured ONCE for the shared mailbox, not
// per source; see internal/connector/imapmail/janitor.go for the rules.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/pipeline"
)

// IMAPJanitorConfig is the run config's imapJanitor block. OFF unless
// enabled.
type IMAPJanitorConfig struct {
	Enabled bool `json:"enabled"`
	// Mailbox is the shared mailbox (default INBOX); every imap source
	// reading it is a sharing source.
	Mailbox string `json:"mailbox,omitempty"`
	// MarkSeen marks processed mail \Seen + Keyword.
	MarkSeen bool   `json:"markSeen"`
	Keyword  string `json:"keyword,omitempty"`
	// DeleteAfterDays deletes mail older than this (default 0 = never; 30 is
	// the recommended value). It must be beyond every sharing source's
	// lookback plus a day.
	DeleteAfterDays int `json:"deleteAfterDays"`
	// DryRun defaults to TRUE: a janitor only changes the mailbox once
	// dryRun is explicitly false (the first live run would permanently
	// delete the whole backlog older than deleteAfterDays).
	DryRun *bool `json:"dryRun,omitempty"`
	// IntervalMinutes between passes (default 15).
	IntervalMinutes int `json:"intervalMinutes,omitempty"`
}

// dryRun is DryRun with its default, true.
func (jc *IMAPJanitorConfig) dryRun() bool { return jc.DryRun == nil || *jc.DryRun }

// imapConn is one imap source's connector and the ingester wrapping it.
type imapConn struct {
	conn *imapmail.Connector
}

// registerIMAP records an imap connector for the janitor, by source id.
func (o categoryOpts) registerIMAP(c *imapmail.Connector) {
	if o.imapConns != nil {
		(*o.imapConns)[c.SourceID()] = imapConn{conn: c}
	}
}

// sameMailbox: INBOX is case-insensitive (RFC 3501 5.1), every other
// mailbox name is case-sensitive.
func sameMailbox(a, b string) bool {
	if strings.EqualFold(a, "INBOX") || strings.EqualFold(b, "INBOX") {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// buildJanitor builds the janitor from the run config and the built imap
// sources, or nil when it is off. ingesters give each source's outer
// completeness (a promo source that deferred work, a wine wrapper).
func buildJanitor(jc *IMAPJanitorConfig, conns map[string]imapConn, ingesters []*pipeline.Ingester, logf func(string, ...any)) (*imapmail.Janitor, error) {
	if jc == nil || !jc.Enabled {
		return nil, nil
	}
	mailbox := jc.Mailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}
	var sources []imapmail.JanitorSource
	maxLookback := 0
	for _, ing := range ingesters {
		ic, ok := conns[ing.SourceID()]
		if !ok || !sameMailbox(ic.conn.MailboxName(), mailbox) {
			continue
		}
		maxLookback = max(maxLookback, ic.conn.LookbackDays())
		outer, conn := ing.Connector, ic.conn
		sources = append(sources, imapmail.JanitorSource{
			Name: ing.SourceID(), Addresses: conn.Addresses(),
			Report: func() imapmail.FetchReport {
				r := conn.Report()
				if rc, ok := outer.(interface{ FetchComplete() bool }); ok && !rc.FetchComplete() {
					r.Complete = false
				}
				return r
			},
		})
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("imapJanitor: no imap source reads mailbox %q", mailbox)
	}
	if jc.DeleteAfterDays < 0 {
		return nil, fmt.Errorf("imapJanitor: deleteAfterDays must not be negative")
	}
	if jc.DeleteAfterDays > 0 && jc.DeleteAfterDays <= maxLookback+1 {
		return nil, fmt.Errorf("imapJanitor: deleteAfterDays %d must exceed the largest lookback of the sources sharing %q (%d days) plus one day",
			jc.DeleteAfterDays, mailbox, maxLookback)
	}
	if jc.IntervalMinutes < 0 {
		return nil, fmt.Errorf("imapJanitor: intervalMinutes must not be negative")
	}
	return imapmail.NewJanitor(imapmail.JanitorConfig{
		Host: envOr("NAGUS_IMAP_HOST", ""), Port: envOr("NAGUS_IMAP_PORT", ""), TLS: envOr("NAGUS_IMAP_TLS", ""),
		Username: envOr("NAGUS_IMAP_USERNAME", ""), Password: envOr("NAGUS_IMAP_PASSWORD", ""),
		Mailbox: mailbox, MarkSeen: jc.MarkSeen, Keyword: jc.Keyword, DeleteAfterDays: jc.DeleteAfterDays,
		DryRun: jc.dryRun(), Sources: sources, Logf: logf,
	})
}

// runJanitor runs the janitor every interval until ctx is done. The first
// pass waits one interval, so the sources have polled first.
func runJanitor(ctx context.Context, j *imapmail.Janitor, interval time.Duration, logf func(string, ...any)) {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := j.Run(ctx); err != nil && logf != nil {
				logf("%v", err)
			}
		}
	}
}

// writeJanitorMetrics renders the janitor's counters.
func writeJanitorMetrics(w io.Writer, j *imapmail.Janitor) {
	if j == nil {
		return
	}
	st := j.Stats()
	var b strings.Builder
	for _, m := range []struct {
		name, help, typ string
		v               int64
	}{
		{"nagus_imap_janitor_runs_total", "Mailbox janitor passes.", "counter", st.Runs},
		{"nagus_imap_janitor_errors_total", "Mailbox janitor passes that failed.", "counter", st.Errors},
		{"nagus_imap_janitor_marked_total", "Messages the janitor marked read and processed.", "counter", st.Marked},
		{"nagus_imap_janitor_deleted_total", "Messages deleted by the janitor (older than deleteAfterDays).", "counter", st.Deleted},
		{"nagus_imap_janitor_dry_run_marked_total", "Messages a dry-run janitor would have marked.", "counter", st.WouldMark},
		{"nagus_imap_janitor_dry_run_deleted_total", "Messages a dry-run janitor would have deleted.", "counter", st.WouldDelete},
		{"nagus_imap_janitor_delete_skipped_total", "Delete passes skipped because the server has no UID EXPUNGE (UIDPLUS).", "counter", st.DeleteSkipped},
	} {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", m.name, m.help, m.name, m.typ, m.name, m.v)
	}
	last := int64(0)
	if !st.LastRun.IsZero() {
		last = st.LastRun.Unix()
	}
	fmt.Fprintf(&b, "# HELP nagus_imap_janitor_last_run_timestamp_seconds When the janitor last ran.\n# TYPE nagus_imap_janitor_last_run_timestamp_seconds gauge\nnagus_imap_janitor_last_run_timestamp_seconds %d\n", last)
	_, _ = io.WriteString(w, b.String())
}
