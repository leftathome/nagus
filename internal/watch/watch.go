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

	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
)

// Watch is a saved query plus a strong-match threshold. Audience is an opaque
// routing tag that openclaw's resolver interprets (nagus never interprets it).
type Watch struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Text     string `json:"text,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Audience string `json:"audience,omitempty"`

	// StrongVerdicts marks which deal verdicts count as a strong match (ping).
	// Defaults to ["great"] when both this and MinScore are unset.
	StrongVerdicts []string `json:"strong_verdicts,omitempty"`
	// MinScore, when > 0, additionally marks any item scoring >= it as strong.
	MinScore float64 `json:"min_score,omitempty"`
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

// Evaluate runs one watch over the pipeline's surface (query -> hard-filter ->
// enrich -> score -> rank) and partitions the ranked results into candidates and
// strong matches.
func Evaluate(ctx context.Context, p *pipeline.Pipeline, w Watch) (Result, error) {
	sr, err := p.Surface(ctx, store.Query{Category: w.Category, Text: w.Text, Limit: w.Limit})
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

// EvaluateAll evaluates every watch in the config. A single watch's evaluation
// error aborts (the query is deterministic; a Surface error is a store fault,
// not per-watch noise).
func EvaluateAll(ctx context.Context, p *pipeline.Pipeline, cfg Config) ([]Result, error) {
	out := make([]Result, 0, len(cfg.Watches))
	for _, w := range cfg.Watches {
		r, err := Evaluate(ctx, p, w)
		if err != nil {
			return nil, fmt.Errorf("watch %q: %w", w.Name, err)
		}
		out = append(out, r)
	}
	return out, nil
}
