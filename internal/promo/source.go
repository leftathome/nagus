package promo

import (
	"context"

	"github.com/leftathome/nagus/internal/listing"
)

// Source wraps a promo source's imap connector: each fetch starts a fresh
// per-poll budget, the poll's products are deduped by product (the newest
// mail wins), and the fetch is reported incomplete when work was deferred,
// so no offer expires for want of a read nagus did not do.
type Source struct {
	Inner  listing.Connector
	Parser *Parser
}

// SourceID is the inner connector's.
func (s *Source) SourceID() string { return s.Inner.SourceID() }

// Fetch runs one poll.
func (s *Source) Fetch(ctx context.Context) ([]listing.Raw, error) {
	s.Parser.startPoll()
	raws, err := s.Inner.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	for i := range raws {
		// A retailer's Message-ID is written by its mailing service and can
		// encode subscriber and campaign ids: the offer keeps a short,
		// non-reversible tag of it instead.
		if id, ok := raws[i].Aspects[AspectMessageID]; ok && id != "" {
			raws[i].Aspects[AspectMessageID] = msgTag(id)
		}
	}
	return Dedupe(raws), nil
}

// FetchComplete is the inner connector's completeness AND no deferred work.
func (s *Source) FetchComplete() bool {
	if s.Parser.incomplete() {
		return false
	}
	if rc, ok := s.Inner.(interface{ FetchComplete() bool }); ok {
		return rc.FetchComplete()
	}
	return true
}

// Dedupe keeps one listing per source key: the one whose mail is newest
// (latest EndsAt, which is arrival + lookback), first seen on a tie. Order
// is otherwise preserved.
func Dedupe(raws []listing.Raw) []listing.Raw {
	idx := map[string]int{}
	out := make([]listing.Raw, 0, len(raws))
	for _, r := range raws {
		if i, ok := idx[r.SourceKey]; ok {
			if r.EndsAt.After(out[i].EndsAt) {
				out[i] = r
			}
			continue
		}
		idx[r.SourceKey] = len(out)
		out = append(out, r)
	}
	return out
}

// Metrics are the counters this source reports to.
func (s *Source) Metrics() *Metrics { return s.Parser.cfg.Metrics }
