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

// seedHDDItems seeds the same four-drive hdd corpus shared by
// mkHDDSurface (used10/new16/refurb8 survive the capacity floor; small4 does
// not).
func seedHDDItems(t *testing.T, st store.Store) {
	t.Helper()
	putItem(t, st, "used10", "10", 8950)
	putItem(t, st, "new16", "16", 27999)
	putItem(t, st, "refurb8", "8", 12999)
	putItem(t, st, "small4", "4", 4000)
}

// verdictByID drives deterministic scoring in tests.
var verdictByID = map[string]string{
	"used10":  "great",
	"new16":   "good",
	"refurb8": "poor",
	"small4":  "great", // would be strong, but is filtered out by capacity floor
}

// mkHDDSurface builds a *pipeline.Surface over the seeded hdd corpus (see
// seedHDDItems): the read half that both single-watch Evaluate and
// EvaluateAll dispatch to.
func mkHDDSurface(t *testing.T) *pipeline.Surface {
	t.Helper()
	st := store.NewMemoryStore()
	seedHDDItems(t, st)
	return &pipeline.Surface{
		Store:  st,
		Filter: score.Filter{Category: "hdd", RequirePriced: true, MinAttr: map[string]float64{"capacity_tb": 8}},
		Valuate: func(_ context.Context, it item.Item) (score.DealSignal, error) {
			v := verdictByID[it.ID]
			return score.DealSignal{Verdict: v, HasReference: true, Ratio: 1}, nil
		},
	}
}

// putLandItem seeds one land parcel (distinct category from putItem's hdd).
func putLandItem(t *testing.T, st store.Store, id, acres string, cents int64) {
	t.Helper()
	it := item.Item{
		ID: id, Category: "land", Class: item.ClassDurable, Title: id,
		PriceCents: cents, Currency: "USD", SourceID: "test", SourceKey: id,
		SeenAt: time.Unix(1000, 0), Attributes: map[string]string{"acreage": acres},
	}
	if err := st.Put(context.Background(), it); err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
}

// landSurfaceReturningOne is a land surface over a store seeded with exactly one
// land item and a pass-all (category-only) filter; used to prove EvaluateAll
// dispatches a land watch here, not to the hdd surface.
func landSurfaceReturningOne(t *testing.T) *pipeline.Surface {
	t.Helper()
	st := store.NewMemoryStore()
	putLandItem(t, st, "parcel5", "5", 500000)
	return &pipeline.Surface{
		Store:  st,
		Filter: score.Filter{Category: "land"},
		Valuate: func(_ context.Context, _ item.Item) (score.DealSignal, error) {
			return score.DealSignal{Verdict: "great", HasReference: true, Ratio: 1}, nil
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
	p := mkHDDSurface(t)
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
	p := mkHDDSurface(t)
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
	p := mkHDDSurface(t)
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
	p := mkHDDSurface(t)
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
	surfaces := map[string]*pipeline.Surface{"hdd": mkHDDSurface(t)}
	cfg := Config{Watches: []Watch{
		{Name: "big-deals", Category: "hdd"},
		{Name: "any-good", Category: "hdd", StrongVerdicts: []string{"great", "good"}},
	}}
	rs, err := EvaluateAll(context.Background(), surfaces, cfg, inqNow)
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

// TestEvaluateAllDispatchesByCategory proves each watch is evaluated by the
// surface for ITS category: the land watch resolves to the land surface (its one
// land parcel), never the hdd surface's drives.
func TestEvaluateAllDispatchesByCategory(t *testing.T) {
	surfaces := map[string]*pipeline.Surface{
		"hdd":  mkHDDSurface(t),
		"land": landSurfaceReturningOne(t),
	}
	cfg := Config{Watches: []Watch{
		{Name: "land-watch", Category: "land"},
		{Name: "hdd-watch", Category: "hdd"},
	}}
	rs, err := EvaluateAll(context.Background(), surfaces, cfg, inqNow)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("want 2 results, got %d", len(rs))
	}
	land := rs[0]
	if land.Watch.Name != "land-watch" || len(land.Candidates) != 1 || land.Candidates[0].Item.Category != "land" {
		t.Fatalf("land watch = %v, want exactly one land candidate", ids(land.Candidates))
	}
	hdd := rs[1]
	// hdd surface: 3 survivors (small4 is under the capacity floor).
	if len(hdd.Candidates) != 3 {
		t.Fatalf("hdd watch candidates=%v, want 3", ids(hdd.Candidates))
	}
}

// TestEvaluateAllUnknownCategoryErrors: a watch naming a category with no
// configured surface is a per-watch error (it cannot silently surface nothing).
func TestEvaluateAllUnknownCategoryErrors(t *testing.T) {
	surfaces := map[string]*pipeline.Surface{"hdd": mkHDDSurface(t)}
	cfg := Config{Watches: []Watch{{Name: "ghost", Category: "nope"}}}
	if _, err := EvaluateAll(context.Background(), surfaces, cfg, inqNow); err == nil {
		t.Fatal("expected error for a watch naming an unconfigured category")
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

// --- inquiries: duration and principal (nagus-7yq) ----------------------------

var inqNow = time.Unix(1_750_000_000, 0).UTC()

// An inquiry with no expiry is always active. This is what keeps every config
// written before inquiries existed valid and unchanged -- adding a duration
// concept must not silently stop existing watches from working.
func TestInquiryWithoutExpiryIsAlwaysActive(t *testing.T) {
	w := Watch{Name: "forever", Category: "hdd"}
	if !w.Active(inqNow) {
		t.Fatal("an inquiry with no expiry must be active")
	}
	if !w.Active(inqNow.Add(100 * 365 * 24 * time.Hour)) {
		t.Fatal("an inquiry with no expiry must stay active indefinitely")
	}
}

func TestInquiryExpiryBoundsTheSearch(t *testing.T) {
	w := Watch{Name: "timeboxed", Category: "hdd", ExpiresAt: inqNow.Add(time.Hour)}
	if !w.Active(inqNow) {
		t.Error("should be active before expiry")
	}
	if w.Active(inqNow.Add(2 * time.Hour)) {
		t.Error("should be inactive after expiry")
	}
	if w.Active(w.ExpiresAt) {
		t.Error("expiry is exclusive: at the instant it expires it is no longer active")
	}
}

// Principal (who asked) is distinct from Audience (where to deliver). They often
// coincide, which is exactly why they must be separable.
func TestPrincipalIsDistinctFromAudience(t *testing.T) {
	w := Watch{Name: "w", Category: "hdd", Audience: "household", Principal: "steve"}
	if w.Principal == w.Audience {
		t.Fatal("fixture is not exercising the distinction")
	}
	if w.Principal != "steve" || w.Audience != "household" {
		t.Fatalf("principal=%q audience=%q did not round-trip", w.Principal, w.Audience)
	}
}

// A category is ACTIVE only while an unexpired inquiry references it; otherwise
// it is dormant and its machinery is not worth running.
func TestActiveCategoriesTracksUnexpiredInquiriesOnly(t *testing.T) {
	cfg := Config{Watches: []Watch{
		{Name: "live-hdd", Category: "hdd"},
		{Name: "dead-land", Category: "land", ExpiresAt: inqNow.Add(-time.Hour)},
		{Name: "live-land-later", Category: "land", ExpiresAt: inqNow.Add(time.Hour)},
	}}
	got := cfg.ActiveCategories(inqNow)
	if !got["hdd"] || !got["land"] {
		t.Fatalf("active = %v, want both hdd (no expiry) and land (one live inquiry)", got)
	}
	// Once the live land inquiry expires too, land goes dormant.
	later := cfg.ActiveCategories(inqNow.Add(2 * time.Hour))
	if later["land"] {
		t.Errorf("land should be dormant once every land inquiry has expired: %v", later)
	}
	if !later["hdd"] {
		t.Errorf("hdd has no expiry and must stay active: %v", later)
	}
}

// An EXPIRED inquiry produces no result at all -- not an empty one. A want whose
// duration has run out should stop pinging, and an empty result would read as
// "looked, found nothing" rather than "no longer looking".
func TestEvaluateAllSkipsExpiredInquiries(t *testing.T) {
	surfaces := map[string]*pipeline.Surface{"hdd": mkHDDSurface(t)}
	cfg := Config{Watches: []Watch{
		{Name: "live", Category: "hdd"},
		{Name: "expired", Category: "hdd", ExpiresAt: inqNow.Add(-time.Hour)},
	}}
	rs, err := EvaluateAll(context.Background(), surfaces, cfg, inqNow)
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("got %d results, want 1 -- the expired inquiry must be absent, not empty", len(rs))
	}
	if rs[0].Watch.Name != "live" {
		t.Fatalf("wrong inquiry survived: %q", rs[0].Watch.Name)
	}
	// And it comes back once we ask at a time before it expired.
	before, err := EvaluateAll(context.Background(), surfaces, cfg, inqNow.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("got %d results before the expiry, want 2", len(before))
	}
}

// An expired inquiry pointing at a category with no surface must NOT error --
// it is not being evaluated, so its category never needs to exist. Otherwise a
// lapsed want could break the whole evaluation pass.
func TestExpiredInquiryWithUnknownCategoryDoesNotError(t *testing.T) {
	surfaces := map[string]*pipeline.Surface{"hdd": mkHDDSurface(t)}
	cfg := Config{Watches: []Watch{
		{Name: "live", Category: "hdd"},
		{Name: "lapsed", Category: "sneakers", ExpiresAt: inqNow.Add(-time.Hour)},
	}}
	rs, err := EvaluateAll(context.Background(), surfaces, cfg, inqNow)
	if err != nil {
		t.Fatalf("a lapsed inquiry for a dormant category must not break evaluation: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("got %d results, want 1", len(rs))
	}
}
