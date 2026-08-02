// Package watch implements the nagus side of delivery (design sections 10-11):
// a watch is a saved search_items query plus a notify threshold. nagus EVALUATES
// watches over the stored corpus and REPORTS matches -- candidates (everything a
// watch surfaces, destined for the quiet inbox) and strong matches (the rare
// great ones, destined for a ping). It does not deliver: openclaw's cron polls
// these results and routes them through the household/audience resolver. This
// keeps nagus read-only (eyes, not hands): a watch surfaces, it never acts.
package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
)

// Watch is an INQUIRY: a standing want held by a principal (spec: offer/product
// re-architecture, "Inquiries drive category activation"). The type keeps its
// original name, and the config key stays "watches", because a deployed
// ConfigMap depends on both; the concept is the one the spec calls an Inquiry.
//
// An Inquiry is three things:
//   - CRITERIA -- what is wanted (Category + Text + the strong-match threshold).
//   - DURATION -- how long to keep looking, so a want does not search forever.
//     See ExpiresAt.
//   - PRINCIPAL -- who asked, and therefore on whose behalf we are looking. This
//     is NOT the same as Audience: Audience is a delivery routing tag that
//     openclaw's resolver interprets, whereas Principal is the requester. They
//     often coincide today, which is exactly why they need separating before
//     anything depends on the distinction.
type Watch struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Text     string `json:"text,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Audience string `json:"audience,omitempty"`

	// Principal is who requested this inquiry. Opaque to nagus, exactly like
	// Audience -- nagus never interprets either, it only carries them.
	Principal string `json:"principal,omitempty"`
	// ExpiresAt bounds how long to keep looking. ZERO MEANS NO EXPIRY, which
	// keeps every existing watch valid and unchanged: a config written before
	// inquiries existed does not silently stop working.
	ExpiresAt time.Time `json:"expires_at,omitempty"`

	// StrongVerdicts marks which deal verdicts count as a strong match (ping).
	// Defaults to ["great"] when both this and MinScore are unset.
	StrongVerdicts []string `json:"strong_verdicts,omitempty"`
	// MinScore, when > 0, additionally marks any item scoring >= it as strong.
	MinScore float64 `json:"min_score,omitempty"`
}

// Active reports whether this inquiry is still being looked for at time now.
// An inquiry with no expiry is always active.
func (w Watch) Active(now time.Time) bool {
	return w.ExpiresAt.IsZero() || now.Before(w.ExpiresAt)
}

// ActiveCategories returns the categories that at least one ACTIVE inquiry
// references, which is what the spec means by a category being active rather
// than dormant: the machinery for a kind of good is worth running only while
// someone is actually asking about it.
//
// This currently REPORTS activation rather than enforcing it -- surfaces are
// still built from config. Making it load-bearing (dormant categories skip
// evaluation entirely) is the next step, and is deliberately separate so that
// turning it on cannot darken a live surface by surprise.
func (c Config) ActiveCategories(now time.Time) map[string]bool {
	out := map[string]bool{}
	for _, w := range c.Watches {
		if w.Active(now) && w.Category != "" {
			out[w.Category] = true
		}
	}
	return out
}

// Config is a set of saved watches (the "saved queries").
type Config struct {
	Watches []Watch `json:"watches"`
}

// LoadConfig reads a JSON watches file. A missing path is an error; an empty
// file (no watches) is valid.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read watches %q: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse watches %q: %w", path, err)
	}
	for i, w := range c.Watches {
		if w.Name == "" {
			return Config{}, fmt.Errorf("watch #%d: name is required", i)
		}
	}
	return c, nil
}

// isStrong reports whether a scored item clears the watch's ping threshold.
func (w Watch) isStrong(sc pipeline.Scored) bool {
	verdicts := w.StrongVerdicts
	if len(verdicts) == 0 && w.MinScore <= 0 {
		verdicts = []string{"great"} // default threshold
	}
	for _, v := range verdicts {
		if sc.Signal.Verdict == v {
			return true
		}
	}
	if w.MinScore > 0 && sc.Score.Value >= w.MinScore {
		return true
	}
	return false
}

// Result is one watch's evaluation: every surfaced item is a Candidate; the
// subset clearing the threshold are Strong matches. Strong is always a subset of
// Candidates (same ranked order).
type Result struct {
	Watch      Watch
	Candidates []pipeline.Scored
	Strong     []pipeline.Scored
}

// Surfacer is the read half a watch needs: query -> ranked scored results.
// *pipeline.Surface satisfies it.
type Surfacer interface {
	Surface(ctx context.Context, q store.Query) (pipeline.SurfaceResult, error)
}

// Evaluate runs one watch over the surface (query -> hard-filter -> enrich ->
// score -> rank) and partitions the ranked results into candidates and strong
// matches.
func Evaluate(ctx context.Context, s Surfacer, w Watch) (Result, error) {
	sr, err := s.Surface(ctx, store.Query{Category: w.Category, Text: w.Text, Limit: w.Limit})
	if err != nil {
		return Result{}, err
	}
	res := Result{Watch: w, Candidates: sr.Items}
	for _, sc := range sr.Items {
		if w.isStrong(sc) {
			res.Strong = append(res.Strong, sc)
		}
	}
	return res, nil
}

// EvaluateAll evaluates every watch in the config, dispatching each to the
// surface for its category. A watch naming an unconfigured category is an error
// (a saved query must resolve to a real surface). A single watch's evaluation
// error aborts (the query is deterministic; a Surface error is a store fault,
// not per-watch noise).
func EvaluateAll(ctx context.Context, surfaces map[string]*pipeline.Surface, cfg Config, now time.Time) ([]Result, error) {
	out := make([]Result, 0, len(cfg.Watches))
	for _, w := range cfg.Watches {
		// An EXPIRED inquiry is not evaluated and produces no result at all --
		// not an empty one. A want with a duration that has run out should stop
		// pinging, and an empty result would read as "looked, found nothing"
		// rather than "no longer looking".
		if !w.Active(now) {
			continue
		}
		sf, ok := surfaces[w.Category]
		if !ok {
			return nil, fmt.Errorf("watch %q: unknown category %q", w.Name, w.Category)
		}
		r, err := Evaluate(ctx, sf, w)
		if err != nil {
			return nil, fmt.Errorf("watch %q: %w", w.Name, err)
		}
		out = append(out, r)
	}
	return out, nil
}
