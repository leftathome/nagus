package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
)

func putItem(t *testing.T, st store.Store, id, capTB string, cents int64) {
	t.Helper()
	it := item.Item{
		ID: id, Category: "hdd", Class: item.ClassDurable, Title: id,
		PriceCents: cents, Currency: "USD", SourceID: "test", SourceKey: id,
		SeenAt: time.Unix(1000, 0), Attributes: map[string]string{"capacity_tb": capTB},
	}
	if err := st.Put(context.Background(), it); err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
}

// verdictByID drives deterministic scoring in tests.
var verdictByID = map[string]string{
	"used10":  "great",
	"new16":   "good",
	"refurb8": "poor",
	"small4":  "great", // would be strong, but is filtered out by capacity floor
}

func mkPipeline(t *testing.T) *pipeline.Pipeline {
	t.Helper()
	st := store.NewMemoryStore()
	putItem(t, st, "used10", "10", 8950)
	putItem(t, st, "new16", "16", 27999)
	putItem(t, st, "refurb8", "8", 12999)
	putItem(t, st, "small4", "4", 4000)
	return &pipeline.Pipeline{
		Store:  st,
		Filter: score.Filter{Category: "hdd", RequirePriced: true, MinAttr: map[string]float64{"capacity_tb": 8}},
		Valuate: func(_ context.Context, it item.Item) (score.DealSignal, error) {
			v := verdictByID[it.ID]
			return score.DealSignal{Verdict: v, HasReference: true, Ratio: 1}, nil
		},
	}
}

func ids(scored []pipeline.Scored) []string {
	out := make([]string, len(scored))
	for i, s := range scored {
		out[i] = s.Item.ID
	}
	return out
}

func TestEvaluateDefaultThresholdIsGreat(t *testing.T) {
	p := mkPipeline(t)
	res, err := Evaluate(context.Background(), p, Watch{Name: "w", Category: "hdd"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// small4 is filtered out by the capacity floor, so it never becomes a
	// candidate even though its verdict is "great".
	if got := len(res.Candidates); got != 3 {
		t.Fatalf("candidates=%d (%v), want 3 (small4 filtered)", got, ids(res.Candidates))
	}
	// Default strong threshold = verdict "great": only used10.
	if len(res.Strong) != 1 || res.Strong[0].Item.ID != "used10" {
		t.Fatalf("strong=%v, want [used10]", ids(res.Strong))
	}
}

func TestEvaluateStrongVerdictsList(t *testing.T) {
	p := mkPipeline(t)
	res, err := Evaluate(context.Background(), p, Watch{
		Name: "w", Category: "hdd", StrongVerdicts: []string{"great", "good"},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// great (used10) + good (new16); poor (refurb8) excluded.
	if len(res.Strong) != 2 {
		t.Fatalf("strong=%v, want used10+new16", ids(res.Strong))
	}
}

func TestEvaluateMinScore(t *testing.T) {
	p := mkPipeline(t)
	// great ~100, good ~75, poor ~25. MinScore 80 -> only great.
	res, err := Evaluate(context.Background(), p, Watch{Name: "w", Category: "hdd", MinScore: 80})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(res.Strong) != 1 || res.Strong[0].Item.ID != "used10" {
		t.Fatalf("strong=%v, want [used10] at MinScore 80", ids(res.Strong))
	}
}

func TestEvaluateStrongIsSubsetOfCandidates(t *testing.T) {
	p := mkPipeline(t)
	res, _ := Evaluate(context.Background(), p, Watch{Name: "w", Category: "hdd", StrongVerdicts: []string{"great", "good", "poor"}})
	cand := map[string]bool{}
	for _, c := range res.Candidates {
		cand[c.Item.ID] = true
	}
	for _, s := range res.Strong {
		if !cand[s.Item.ID] {
			t.Fatalf("strong %s not in candidates", s.Item.ID)
		}
	}
}

func TestEvaluateAll(t *testing.T) {
	p := mkPipeline(t)
	cfg := Config{Watches: []Watch{
		{Name: "big-deals", Category: "hdd"},
		{Name: "any-good", Category: "hdd", StrongVerdicts: []string{"great", "good"}},
	}}
	rs, err := EvaluateAll(context.Background(), p, cfg)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if len(rs) != 2 || rs[0].Watch.Name != "big-deals" || rs[1].Watch.Name != "any-good" {
		t.Fatalf("unexpected results: %+v", rs)
	}
	if len(rs[0].Strong) != 1 || len(rs[1].Strong) != 2 {
		t.Fatalf("strong counts: %d, %d; want 1, 2", len(rs[0].Strong), len(rs[1].Strong))
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watches.json")
	body := `{"watches":[{"name":"cheap-big","category":"hdd","min_score":80,"audience":"steve"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Watches) != 1 || cfg.Watches[0].Name != "cheap-big" || cfg.Watches[0].MinScore != 80 {
		t.Fatalf("bad config: %+v", cfg)
	}
}

func TestLoadConfigRejectsNamelessWatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "w.json")
	_ = os.WriteFile(path, []byte(`{"watches":[{"category":"hdd"}]}`), 0o600)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for a watch with no name")
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
