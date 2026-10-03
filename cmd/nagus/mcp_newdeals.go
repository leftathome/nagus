package main

// new_deals and list_watches (nagus-417): what an agent needs for "show me new
// good deals on things I care about". Both READ-ONLY. "New since" is
// STATELESS (operator decision 2026-10-03): the caller passes the time it last
// asked, nagus keeps no cursor, and every answer carries as_of to pass next
// time. Expired listings never appear (the surface hides them).

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/leftathome/go-service-kit/mcp"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
	"github.com/leftathome/nagus/internal/watch"
)

const (
	// newDealsMaxAge bounds how far back since may reach. A forgotten or
	// zero cursor must not turn "what is new" into a dump of the corpus;
	// search_items is the tool for everything.
	newDealsMaxAge = 30 * 24 * time.Hour
	// newDealsFutureSkew tolerates a caller clock slightly ahead of nagus's.
	newDealsFutureSkew   = 5 * time.Minute
	newDealsDefaultLimit = 25
	newDealsMaxLimit     = 100

	// new_reason values.
	reasonFirstSeen = "first_seen"
	reasonPriceDrop = "price_drop"
)

// verdictRank is the deal-verdict ladder min_verdict selects on: a row
// qualifies when its verdict ranks at or above the minimum. Unscored verdicts
// (unknown-no-reference, unknown-no-price, release labels) are not on it and
// qualify only for "any".
var verdictRank = map[string]int{
	score.VerdictGreat:  4,
	score.VerdictGood:   3,
	score.VerdictMarket: 2,
	score.VerdictPoor:   1,
}

// minVerdicts is min_verdict's enum, best first.
var minVerdicts = []string{score.VerdictGreat, score.VerdictGood, score.VerdictMarket, score.VerdictPoor, "any"}

// newDealsArgs is new_deals' argument object.
type newDealsArgs struct {
	Since             string `json:"since"`
	Category          string `json:"category"`
	Watch             string `json:"watch"`
	MinVerdict        string `json:"min_verdict"`
	IncludePriceDrops *bool  `json:"include_price_drops"`
	Limit             *int   `json:"limit"`
}

// listWatchesArgs is list_watches' argument object.
type listWatchesArgs struct {
	Name  string `json:"name"`
	Limit *int   `json:"limit"`
}

// newDealsSchema and listWatchesSchema are pinned by
// TestMCPToolSchemasUnchangedForOpenclaw: a change here is deliberate.
func newDealsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"since": map[string]any{"type": "string", "format": "date-time",
				"description": "RFC 3339 time you last asked (pass the previous answer's as_of, minus an hour of overlap). At most 30 days ago; not in the future."},
			"category": map[string]any{"type": "string",
				"description": "One configured category (hdd, wine, ...). Omit for every category."},
			"watch": map[string]any{"type": "string",
				"description": "A watch name from list_watches: only that watch's category and text, and only rows that clear its strong (ping) criteria."},
			"min_verdict": map[string]any{"type": "string", "enum": minVerdicts,
				"description": "Lowest deal verdict to return; default good. Ladder: great > good > market > poor. any also returns unscored rows."},
			"include_price_drops": map[string]any{"type": "boolean",
				"description": "Default true: a listing seen before since also counts when its price DROPPED at or after since."},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": newDealsMaxLimit},
		},
		"required":             []string{"since"},
		"additionalProperties": false,
	}
}

func listWatchesSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string",
				"description": "One watch's name: returns that watch with its current strong matches as rows. Omit to list every watch with counts only."},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": newDealsMaxLimit,
				"description": "With name: at most this many strong matches (best first); default 25. strong_count is the total."},
		},
		"additionalProperties": false,
	}
}

func (s *server) newDealsTools() []mcp.Tool {
	newDeals := mcp.NewTool(mcp.ToolSpec{
		Name: "new_deals",
		Description: "READ-ONLY. Active listings that are NEW since a time you give: first seen by nagus at or after since, " +
			"or (include_price_drops, default true) whose price DROPPED at or after since. Ranked like search_items, best first; " +
			"expired listings are never returned. nagus keeps no memory of what you have seen: pass since = the as_of of your " +
			"previous call (minus an hour: a conservative margin, because a poll that started before as_of can store its listings after it) " +
			"and drop rows you already showed: key a row on id + price_changed_at when new_reasons contains price_drop, else on id " +
			"alone -- so a listing shown as new returns once more only when its price drops, never when it rises. Each row has new_reasons (first_seen and/or price_drop) and new_reason (price_drop when the price " +
			"dropped at or after since, else first_seen). price_drop means lower than previous_price_cents; at_lowest_price says whether " +
			"it is also the lowest price nagus has seen for the listing (a drop after a rise may not be). Rows are per LISTING: the same product at " +
			"two sellers is two rows -- group them on comparison_key (or product_id when comparison_key is empty) before presenting. " +
			"min_verdict defaults to good. watch restricts to one watch from list_watches: its category, text and strong criteria " +
			"(min_verdict then applies only if you give it; the watch's own limit is not used). " +
			"Needs nagus's offer layer; otherwise it answers invalid argument. " + offerFieldsDescription,
		InputSchema: newDealsSchema(),
		Noun:        "item(s)",
		Note:        mcpUntrustedNote,
		Access:      mcp.ReadOnly,
	}, s.mcpNewDeals)

	listWatches := mcp.NewTool(mcp.ToolSpec{
		Name: "list_watches",
		Description: "READ-ONLY. The household's standing watches (\"things I care about\"): name, category, text, the strong " +
			"criteria that make a match ping (strong_verdicts, min_score, min_discount_pct, new_within_days), audience and " +
			"expires_at, with candidate_count and strong_count over ACTIVE listings right now. Expired watches are not listed. " +
			"Give name to also get that watch's strong matches as rows (with the offer fields of search_items), at most limit " +
			"(default 25) of them; strong_count is the total. " +
			"Use a watch name with new_deals to ask what is new for that watch.",
		InputSchema: listWatchesSchema(),
		Noun:        "watch(es)",
		Note:        mcpUntrustedNote,
		Access:      mcp.ReadOnly,
	}, s.mcpListWatches)
	return []mcp.Tool{newDeals, listWatches}
}

// activeWatch returns the active watch with this name.
func (s *server) activeWatch(name string, now time.Time) (watch.Watch, bool) {
	for _, w := range s.watches.Watches {
		if w.Name == name && w.Active(now) {
			return w, true
		}
	}
	return watch.Watch{}, false
}

func (s *server) mcpNewDeals(ctx context.Context, a newDealsArgs) (mcp.Result, error) {
	if s.offers == nil {
		return mcp.Result{}, mcp.InvalidArgument("new_deals needs the offer layer, which is off on this nagus")
	}
	now := s.clock()
	since, err := time.Parse(time.RFC3339, a.Since)
	if err != nil {
		// Caller input is not echoed back.
		return mcp.Result{}, mcp.InvalidArgument("since must be an RFC 3339 time, e.g. 2026-10-03T12:00:00Z")
	}
	if since.After(now.Add(newDealsFutureSkew)) {
		return mcp.Result{}, mcp.InvalidArgument("since is in the future")
	}
	if since.Before(now.Add(-newDealsMaxAge)) {
		return mcp.Result{}, mcp.InvalidArgument("since is more than 30 days ago; use search_items to browse everything")
	}
	minVerdict := score.VerdictGood
	if a.MinVerdict != "" {
		if !slices.Contains(minVerdicts, a.MinVerdict) {
			return mcp.Result{}, mcp.InvalidArgument("min_verdict must be one of great, good, market, poor, any")
		}
		minVerdict = a.MinVerdict
	}
	limit := newDealsDefaultLimit
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > newDealsMaxLimit {
			return mcp.Result{}, mcp.InvalidArgument("limit must be between 1 and 100")
		}
		limit = *a.Limit
	}
	drops := a.IncludePriceDrops == nil || *a.IncludePriceDrops

	var w *watch.Watch
	text := ""
	var cats []string
	switch {
	case a.Watch != "":
		found, ok := s.activeWatch(a.Watch, now)
		if !ok {
			return mcp.Result{}, mcp.InvalidArgument("no active watch has that name; list_watches names them")
		}
		if _, ok := s.surfaces[found.Category]; !ok {
			return mcp.Result{}, mcp.InvalidArgument("that watch's category is not configured on this nagus")
		}
		if a.Category != "" && a.Category != found.Category {
			return mcp.Result{}, mcp.InvalidArgument("category and the watch's category differ; give one")
		}
		w, text, cats = &found, found.Text, []string{found.Category}
	case a.Category != "":
		if _, ok := s.surfaces[a.Category]; !ok {
			return mcp.Result{}, mcp.InvalidArgument(mcp.Message(
				"unknown category (configured: " + strings.Join(s.categoryNames(), ",") + ")"))
		}
		cats = []string{a.Category}
	default:
		cats = s.categoryNames()
	}

	var picked []pipeline.Scored
	reasons := map[string][]string{}
	hidden, truncated := 0, false
	failed := []string{}
	var lastErr error
	for _, cat := range cats {
		res, err := s.surfaces[cat].Surface(ctx, store.Query{Category: cat, Text: text})
		if err != nil {
			// One category failing must not hide the others' news (as
			// /watches isolates a failing watch). Its rows are simply not
			// in the answer: nothing unchecked is ever surfaced.
			failed, lastErr = append(failed, cat), err
			continue
		}
		hidden += res.HiddenExpired
		truncated = truncated || res.CapReached
		for _, sc := range res.Items {
			why := newReasons(sc.Offer, since, drops)
			if len(why) == 0 {
				continue
			}
			if w != nil {
				if !w.IsStrong(sc) || (a.MinVerdict != "" && !verdictAtLeast(sc.Signal.Verdict, minVerdict)) {
					continue
				}
			} else if !verdictAtLeast(sc.Signal.Verdict, minVerdict) {
				continue
			}
			picked = append(picked, sc)
			reasons[sc.Item.ID] = why
		}
	}
	if len(failed) == len(cats) {
		return mcp.Result{}, lastErr // logged by the kit; the caller sees the fixed message
	}
	// One ranking across categories: score, then id, as Surface ranks.
	sort.SliceStable(picked, func(i, j int) bool {
		if picked[i].Score.Value != picked[j].Score.Value {
			return picked[i].Score.Value > picked[j].Score.Value
		}
		return picked[i].Item.ID < picked[j].Item.ID
	})
	total := len(picked)
	if len(picked) > limit {
		picked = picked[:limit]
	}
	rows := s.rowsFor(ctx, picked)
	for i := range rows {
		why := reasons[rows[i].ID]
		rows[i].NewReasons = why
		// The most recent event leads: a drop after the first sighting.
		rows[i].NewReason = why[len(why)-1]
	}
	// Report what was APPLIED: with a watch and no explicit min_verdict, the
	// watch's strong criteria decide and no verdict floor is applied.
	var applied any = minVerdict
	var watchName any // null without a watch, like min_verdict's null
	if w != nil {
		watchName = w.Name
		if a.MinVerdict == "" {
			applied = nil
		}
	}
	return mcp.Structured(len(rows), map[string]any{
		"as_of":                rfc3339(now),
		"since":                rfc3339(since),
		"categories":           cats,
		"watch":                watchName,
		"min_verdict":          applied,
		"include_price_drops":  drops,
		"new_count":            total,
		"hidden_expired":       hidden,
		"candidates_truncated": truncated,
		"failed_categories":    failed,
		"items":                rows,
	}), nil
}

// newReasons says why a surfaced row is new since the caller's time, in
// event order: first_seen (first sighting at or after since) and/or
// price_drop (a drop at or after since). Both can hold: a listing shown as
// new can drop later, and a caller that already showed it must still learn of
// the drop (it keys a price_drop row on id plus price_changed_at, any other
// row on id alone). nil when the row is not new. Only ACTIVE listings with
// an offer qualify: without one nagus does not know when it first saw it.
func newReasons(sm *offer.Summary, since time.Time, drops bool) []string {
	if sm == nil || sm.Status != offer.StatusActive {
		return nil
	}
	var out []string
	if !sm.FirstSeen.IsZero() && !sm.FirstSeen.Before(since) {
		out = append(out, reasonFirstSeen)
	}
	// PriceDropped needs a previous price, which only a change after the
	// first insert records, so no separate "after first_seen" check.
	if drops && !sm.PriceChangedAt.Before(since) && sm.PriceDropped() {
		out = append(out, reasonPriceDrop)
	}
	return out
}

func verdictAtLeast(verdict, min string) bool {
	if min == "any" {
		return true
	}
	r, ok := verdictRank[verdict]
	return ok && r >= verdictRank[min]
}

// watchInfo is one watch as list_watches reports it. principal is NOT
// reported: it names who asked (possibly a person), /watches does not return
// it either, and an agent has no use for it.
type watchInfo struct {
	Name           string         `json:"name"`
	Category       string         `json:"category"`
	Text           string         `json:"text"`
	Limit          int            `json:"limit,omitempty"`
	Audience       string         `json:"audience,omitempty"`
	ExpiresAt      string         `json:"expires_at,omitempty"`
	StrongCriteria strongCriteria `json:"strong_criteria"`
	// Error is set when this watch could not be evaluated (fixed text).
	Error          string `json:"error,omitempty"`
	CandidateCount *int   `json:"candidate_count,omitempty"`
	StrongCount    *int   `json:"strong_count,omitempty"`
}

// strongCriteria is what makes a watch match ping. A match qualifies when ANY
// criterion holds.
type strongCriteria struct {
	Verdicts       []string `json:"strong_verdicts"`
	MinScore       float64  `json:"min_score,omitempty"`
	MinDiscountPct float64  `json:"min_discount_pct,omitempty"`
	NewWithinDays  int      `json:"new_within_days,omitempty"`
}

func describeWatch(w watch.Watch) watchInfo {
	verdicts := w.EffectiveStrongVerdicts()
	if verdicts == nil {
		verdicts = []string{}
	}
	return watchInfo{
		Name: w.Name, Category: w.Category, Text: w.Text, Limit: w.Limit, Audience: w.Audience,
		ExpiresAt: rfc3339(w.ExpiresAt),
		StrongCriteria: strongCriteria{
			Verdicts: verdicts, MinScore: w.MinScore, MinDiscountPct: w.MinDiscountPct, NewWithinDays: w.NewWithinDays,
		},
	}
}

func (s *server) mcpListWatches(ctx context.Context, a listWatchesArgs) (mcp.Result, error) {
	now := s.clock()
	if a.Name == "" && a.Limit != nil {
		return mcp.Result{}, mcp.InvalidArgument("limit applies only with name (the list form returns no matches)")
	}
	if a.Name != "" {
		w, ok := s.activeWatch(a.Name, now)
		if !ok {
			return mcp.NotFound(), nil
		}
		limit := newDealsDefaultLimit
		if a.Limit != nil {
			if *a.Limit < 1 || *a.Limit > newDealsMaxLimit {
				return mcp.Result{}, mcp.InvalidArgument("limit must be between 1 and 100")
			}
			limit = *a.Limit
		}
		info := describeWatch(w)
		strong := []searchRow{}
		if sf, ok := s.surfaces[w.Category]; !ok {
			info.Error = "this watch's category is not configured on this nagus"
		} else {
			res, err := watch.Evaluate(ctx, sf, w)
			if err != nil {
				return mcp.Result{}, err
			}
			c, n := len(res.Candidates), len(res.Strong)
			info.CandidateCount, info.StrongCount = &c, &n
			// A watch with no limit of its own can match thousands of rows;
			// the agent gets the best few, and strong_count says how many.
			top := res.Strong
			if len(top) > limit {
				top = top[:limit]
			}
			strong = s.rowsFor(ctx, top)
		}
		return mcp.Structured(len(strong), map[string]any{"watch": info, "strong": strong, "strong_returned": len(strong)}), nil
	}
	out := []watchInfo{}
	for _, w := range s.watches.Watches {
		if !w.Active(now) {
			continue
		}
		info := describeWatch(w)
		sf, ok := s.surfaces[w.Category]
		if !ok {
			info.Error = "this watch's category is not configured on this nagus"
			out = append(out, info)
			continue
		}
		res, err := watch.Evaluate(ctx, sf, w)
		if err != nil {
			// One watch failing must not hide the others (as /watches).
			info.Error = "evaluation failed for this watch"
			out = append(out, info)
			continue
		}
		c, n := len(res.Candidates), len(res.Strong)
		info.CandidateCount, info.StrongCount = &c, &n
		out = append(out, info)
	}
	return mcp.Structured(len(out), map[string]any{"watches": out}), nil
}
