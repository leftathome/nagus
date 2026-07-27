# nagus dgq — multi-source / multi-category deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let ONE nagus deployment ingest from N sources and serve M categories over one shared store, collapsing the two HelmReleases (`nagus` + `nagus-land`) into one.

**Architecture:** Split the welded `pipeline.Pipeline` into an `Ingester` (one per source: connector→sanitize→extract→store + freshness purge) and a `Surface` (one per category: store→filter→valuate→score→rank). `cmd/nagus` holds `ingesters []*Ingester` + `surfaces map[string]*Surface` over one `store.Store`, driven by a mounted `config.json` (`sources[]` + `categories{}`). Each source ingests on its own goroutine/interval with per-source failure isolation; reads dispatch by category. Materialized typed items and the existing read path are unchanged — no offer store, no inquiries (those are later slices).

**Tech Stack:** Go 1.26, stdlib only (net/http, encoding/json, flag). Tests: `go test ./... -count=1 -race`, `go vet ./...`, staticcheck. Store: `MemoryStore` reference contract. Chart: Helm (charts/nagus). Deploy: Flux via steve/gitops.

**Spec:** `docs/superpowers/specs/2026-07-26-nagus-offer-product-rearchitecture.md` (slice 1).

**Standing constraints (do not violate):** surface-only/read-only; all listing free text crosses the sanitizer (glovebox) before use; no secrets in git (Vault→ExternalSecret); no emoji in code/strings; storage stays an adapter; MemoryStore is the contract new behavior is tested against.

---

## File Structure

**internal/pipeline/** (split the welded struct)
- Create `ingester.go` — `Ingester` struct + `Ingest()` (the front half + purge). One responsibility: turn a source into stored items.
- Create `surface.go` — `Surface` struct + `Surface()` (the back half). One responsibility: query→filter→valuate→score→rank.
- Create `ingester_test.go`, `surface_test.go` — moved/adapted from `pipeline_test.go`.
- Modify `pipeline.go` — during migration `Pipeline` delegates to the new types; deleted in the final task.

**internal/watch/**
- Modify `watch.go` — `Evaluate` takes a `Surfacer` interface; `EvaluateAll` dispatches each watch by `Category` to a `surfaces map[string]*pipeline.Surface`.
- Modify `watch_test.go` — add cross-category dispatch test.

**cmd/nagus/**
- Create `config.go` — `Config`/`SourceConfig`/`CategoryConfig` types + `LoadRunConfig(path)` + env fallback.
- Create `config_test.go`.
- Modify `category.go` — `buildIngester(SourceConfig, store, opts)` + `buildSurface(cat, CategoryConfig, store, opts)`; delete `buildPipeline`/`buildSourceConnector` in the final task.
- Modify `serve.go` — `server` holds `ingesters`/`surfaces`; `runServe` builds them from config; `runIngestLoop` runs one goroutine per source; `handleSearch`/`handleMetrics`/`handleWatches` updated.
- Modify `mcp.go` — `mcpSearchItems` dispatches by category.
- Modify `main.go` — `runIngest`/`runSearch` subcommands migrated off `Pipeline` to `Ingester`/`Surface` (Task 8).
- Modify `serve_test.go`, `mcp_test.go` — construct the new `server`.

**charts/nagus/**
- Modify `values.yaml` — `category` scalar → `sources[]` + `categories{}` (+ optional `defaultCategory`).
- Create `templates/configmap-config.yaml` — render `config.json`.
- Modify `templates/deployment.yaml` — mount the config file; wire per-source secret envs.

**(separate repo) steve/gitops** — collapse `helmrelease-nagus.yaml` + `helmrelease-nagus-land.yaml` into one. Deploy task; verification only here.

---

## Task 0: Branch

- [ ] **Step 1: Create the working branch** (dgq is non-trivial → branch + MR per repo policy)

```bash
cd /mnt/c/Users/steve/Code/nagus
git checkout -b feat/nagus-dgq-multi-source
git status
```
Expected: `On branch feat/nagus-dgq-multi-source`, clean.

---

## Task 1: `Ingester` type (extract the ingest half)

**Files:**
- Create: `internal/pipeline/ingester.go`
- Create: `internal/pipeline/ingester_test.go`
- Modify: `internal/pipeline/pipeline.go` (Ingest delegates)

- [ ] **Step 1: Write `ingester.go`** with the type and a delegating `Ingest`. Move the ingest logic verbatim from `Pipeline.Ingest`.

```go
package pipeline

import (
	"context"
	"time"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/store"
)

// Ingester runs the front half of the spine for ONE source:
//
//	Connector -> Sanitizer -> Extractor -> Store  (+ freshness purge)
//
// One Ingester == one source. Multiple Ingesters share a Store; each purges only
// its own source's stale content (scoped by Connector.SourceID()). This is the
// unit cmd/nagus runs one-per-source on its own interval.
type Ingester struct {
	Connector listing.Connector
	Sanitizer listing.Sanitizer
	Extractor listing.Extractor
	Store     store.Store

	// StaleAfter, when > 0, enables a post-ingest freshness purge of this
	// source's items older than the window (eBay License 8.1(b)). 0 disables it.
	StaleAfter time.Duration
	// Now returns the current time for the purge cutoff; nil defaults to time.Now.
	Now func() time.Time
	// Logf is an optional log sink; nil disables logging.
	Logf func(format string, args ...any)
}

func (i *Ingester) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Ingester) logf(format string, args ...any) {
	if i.Logf != nil {
		i.Logf(format, args...)
	}
}

// SourceID is the identity of the source this Ingester pulls (its connector's).
func (i *Ingester) SourceID() string { return i.Connector.SourceID() }

// Ingest fetches, sanitizes, extracts, and stores one batch, then purges this
// source's stale items. A per-listing failure is recorded as a Skip and does not
// abort the batch; only a connector Fetch error aborts.
func (i *Ingester) Ingest(ctx context.Context) (IngestResult, error) {
	raws, err := i.Connector.Fetch(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	res := IngestResult{Fetched: len(raws)}
	for _, r := range raws {
		san, err := i.Sanitizer.Sanitize(ctx, r)
		if err != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "sanitize", Reason: err.Error()})
			i.logf("ingest: sanitize dropped %s: %v", r.SourceKey, err)
			continue
		}
		it, err := i.Extractor.Extract(ctx, san)
		if err != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "extract", Reason: err.Error()})
			i.logf("ingest: extract dropped %s: %v", r.SourceKey, err)
			continue
		}
		if err := i.Store.Put(ctx, it); err != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "store", Reason: err.Error()})
			i.logf("ingest: store dropped %s: %v", r.SourceKey, err)
			continue
		}
		res.Stored++
	}
	if i.StaleAfter > 0 && i.Connector != nil {
		cutoff := i.now().Add(-i.StaleAfter)
		purged, derr := i.Store.DeleteStale(ctx, i.Connector.SourceID(), cutoff)
		if derr != nil {
			i.logf("ingest: purge stale %s failed: %v", i.Connector.SourceID(), derr)
		} else {
			res.Purged = purged
			if purged > 0 {
				i.logf("ingest: purged %d stale %s items older than %s", purged, i.Connector.SourceID(), i.StaleAfter)
			}
		}
	}
	return res, nil
}
```

Note: `IngestResult`, `Skip` stay defined in `pipeline.go` (shared) — do not redefine them here.

- [ ] **Step 2: Make `Pipeline.Ingest` delegate** so existing callers/tests keep passing. In `pipeline.go`, replace the body of `Pipeline.Ingest` with:

```go
func (p *Pipeline) Ingest(ctx context.Context) (IngestResult, error) {
	ing := &Ingester{
		Connector: p.Connector, Sanitizer: p.Sanitizer, Extractor: p.Extractor,
		Store: p.Store, StaleAfter: p.StaleAfter, Now: p.Now, Logf: p.Logf,
	}
	return ing.Ingest(ctx)
}
```

- [ ] **Step 3: Write `ingester_test.go`** — copy the ingest tests from `pipeline_test.go` (`TestIngestStoresAndSkips`, `TestIngestConnectorErrorAborts`, `TestIngestPurgesStaleSourceItems`) and retarget them at `&Ingester{...}` instead of `&Pipeline{...}`. Reuse the existing `fakeConnector`/`fakeExtractor`/`raw` helpers (same package, already in `pipeline_test.go`). Example for the first:

```go
func TestIngesterStoresAndSkips(t *testing.T) {
	raws := []listing.Raw{
		raw("a", "Seagate 16TB", 12000, "16"),
		raw("bad", "broken listing", 9999, "8"),
		raw("b", "WD 8TB", 8000, "8"),
	}
	st := store.NewMemoryStore()
	ing := &Ingester{Connector: fakeConnector{raws: raws}, Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest error: %v", err)
	}
	if res.Fetched != 3 || res.Stored != 2 {
		t.Fatalf("Fetched=%d Stored=%d, want 3/2", res.Fetched, res.Stored)
	}
	if len(res.Skips) != 1 || res.Skips[0].Stage != "extract" || res.Skips[0].SourceKey != "bad" {
		t.Fatalf("expected one extract skip for 'bad', got %+v", res.Skips)
	}
}
```
Add the purge test too (adapt `TestIngestPurgesStaleSourceItems`, injecting `Now`). Leave the originals in `pipeline_test.go` for now (they still pass via delegation).

- [ ] **Step 4: Run the pipeline tests**

Run: `go test ./internal/pipeline/ -count=1 -race`
Expected: PASS (new `Ingester*` tests + existing delegating `Pipeline` tests).

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/ingester.go internal/pipeline/ingester_test.go internal/pipeline/pipeline.go
git commit -m "refactor(pipeline): extract Ingester (ingest half); Pipeline.Ingest delegates"
```

---

## Task 2: `Surface` type (extract the surface half)

**Files:**
- Create: `internal/pipeline/surface.go`
- Create: `internal/pipeline/surface_test.go`
- Modify: `internal/pipeline/pipeline.go` (Surface delegates)

- [ ] **Step 1: Write `surface.go`.** Move the surface logic verbatim from `Pipeline.Surface`. Keep the method named `Surface` (type `Surface`, method `Surface` — legal Go; it matches the spec and every existing caller name).

```go
package pipeline

import (
	"context"
	"sort"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
)

// Surface runs the back half of the spine for ONE category:
//
//	Store -> HARD-FILTER -> ENRICH (valuate) -> SCORE -> rank best-first
//
// One Surface == one category (its Filter/Valuate are category-specific). The
// hard-filter runs BEFORE enrich so paid work touches only survivors (ordering
// invariant). Read-only: eyes, not hands.
type Surface struct {
	Store   store.Store
	Filter  score.Filter
	Valuate func(ctx context.Context, it item.Item) (score.DealSignal, error)
	Logf    func(format string, args ...any)
}

func (s *Surface) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Surface queries the stored corpus and returns ranked, scored survivors.
func (s *Surface) Surface(ctx context.Context, q store.Query) (SurfaceResult, error) {
	items, err := s.Store.Search(ctx, q)
	if err != nil {
		return SurfaceResult{}, err
	}
	out := SurfaceResult{Matched: len(items)}
	for _, it := range items {
		if ok, reason := s.Filter.Pass(it); !ok {
			s.logf("surface: filtered %s: %s", it.ID, reason)
			continue
		}
		sig := score.DealSignal{Verdict: "unknown-no-reference"}
		if s.Valuate != nil {
			v, verr := s.Valuate(ctx, it)
			if verr != nil {
				s.logf("surface: valuate failed %s: %v", it.ID, verr)
			} else {
				sig = v
			}
		}
		out.Items = append(out.Items, Scored{Item: it, Signal: sig, Score: score.ScoreItem(it, sig)})
	}
	out.Filtered = len(out.Items)
	sort.SliceStable(out.Items, func(a, b int) bool {
		if out.Items[a].Score.Value != out.Items[b].Score.Value {
			return out.Items[a].Score.Value > out.Items[b].Score.Value
		}
		return out.Items[a].Item.ID < out.Items[b].Item.ID
	})
	return out, nil
}
```

`Scored`, `SurfaceResult` stay in `pipeline.go` (shared) — do not redefine.

- [ ] **Step 2: Make `Pipeline.Surface` delegate.** Replace the body of `Pipeline.Surface` in `pipeline.go` with:

```go
func (p *Pipeline) Surface(ctx context.Context, q store.Query) (SurfaceResult, error) {
	s := &Surface{Store: p.Store, Filter: p.Filter, Valuate: p.Valuate, Logf: p.Logf}
	return s.Surface(ctx, q)
}
```
Remove now-unused imports from `pipeline.go` if `sort` is no longer referenced there (the compiler will tell you).

- [ ] **Step 3: Write `surface_test.go`** — adapt `TestSurfaceFilterBeforeEnrichAndRank` and `TestSurfaceNilValuateDegrades` from `pipeline_test.go` to build `&Surface{...}` directly. Seed the store via an `Ingester` (or `store.Put`) rather than `Pipeline`. Example skeleton:

```go
func TestSurfaceFilterBeforeEnrichAndRank(t *testing.T) {
	st := store.NewMemoryStore()
	ing := &Ingester{Connector: fakeConnector{raws: []listing.Raw{
		raw("a", "Seagate 16TB", 12000, "16"),
		raw("b", "WD 8TB", 8000, "8"),
	}}, Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := &Surface{Store: st, Filter: /* the same Filter the existing test used */}
	res, err := s.Surface(context.Background(), store.Query{Category: "hdd"})
	// ... same assertions as the original test
}
```
Reuse whatever `score.Filter` / `Valuate` fakes the original `pipeline_test.go` used (copy them verbatim so behavior matches).

- [ ] **Step 4: Run**

Run: `go test ./internal/pipeline/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/surface.go internal/pipeline/surface_test.go internal/pipeline/pipeline.go
git commit -m "refactor(pipeline): extract Surface (surface half); Pipeline.Surface delegates"
```

---

## Task 3: `watch.Evaluate` takes a `Surfacer` interface

Loosens the coupling so a watch can be evaluated by a `*pipeline.Surface` (or, transitionally, a `*pipeline.Pipeline`).

**Files:**
- Modify: `internal/watch/watch.go`

- [ ] **Step 1: Introduce the interface and retarget `Evaluate`.** In `watch.go`, add near the top of the type declarations:

```go
// Surfacer is the read half a watch needs: query -> ranked scored results.
// Both *pipeline.Surface and (transitionally) *pipeline.Pipeline satisfy it.
type Surfacer interface {
	Surface(ctx context.Context, q store.Query) (pipeline.SurfaceResult, error)
}
```
Change `Evaluate`'s signature from `p *pipeline.Pipeline` to `s Surfacer` and call `s.Surface(...)` (body otherwise unchanged):

```go
func Evaluate(ctx context.Context, s Surfacer, w Watch) (Result, error) {
	sr, err := s.Surface(ctx, store.Query{Category: w.Category, Text: w.Text, Limit: w.Limit})
	// ... unchanged
}
```

- [ ] **Step 2: Keep `EvaluateAll` compiling** — it still takes `*pipeline.Pipeline` for now and passes it as a `Surfacer` (it satisfies the interface). No signature change in this task.

- [ ] **Step 3: Run**

Run: `go test ./internal/watch/ ./cmd/nagus/ -count=1 -race`
Expected: PASS (no behavior change; `*Pipeline` satisfies `Surfacer`).

- [ ] **Step 4: Commit**

```bash
git add internal/watch/watch.go
git commit -m "refactor(watch): Evaluate takes a Surfacer interface"
```

---

## Task 4: Run config schema + loader

**Files:**
- Create: `cmd/nagus/config.go`
- Create: `cmd/nagus/config_test.go`

- [ ] **Step 1: Write the failing test** `config_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRunConfigParsesSourcesAndCategories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "sources": [
	    {"name":"ebay","category":"hdd","type":"ebay","query":"internal hard drive","intervalMinutes":30,"secretRef":"ebay"},
	    {"name":"cl-seattle","category":"land","type":"craigslist","city":"seattle","clCategory":"rea","intervalMinutes":60}
	  ],
	  "categories": {"hdd":{"minCapacityTB":8},"land":{"minAcreageAcres":1,"budgetCents":0}}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRunConfig(path)
	if err != nil {
		t.Fatalf("LoadRunConfig: %v", err)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("sources=%d want 2", len(cfg.Sources))
	}
	if cfg.Sources[0].Name != "ebay" || cfg.Sources[0].Category != "hdd" || cfg.Sources[0].IntervalMinutes != 30 {
		t.Fatalf("source0 = %+v", cfg.Sources[0])
	}
	if _, ok := cfg.Categories["land"]; !ok {
		t.Fatal("missing land category")
	}
}

func TestLoadRunConfigRejectsUnknownCategoryRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"sources":[{"name":"x","category":"ghost","type":"ebay"}],"categories":{"hdd":{}}}`
	_ = os.WriteFile(path, []byte(body), 0o600)
	if _, err := LoadRunConfig(path); err == nil {
		t.Fatal("expected error: source references a category not in categories{}")
	}
}

func TestLoadRunConfigRejectsDuplicateSourceName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"sources":[{"name":"dup","category":"hdd","type":"ebay"},{"name":"dup","category":"hdd","type":"ebay"}],"categories":{"hdd":{}}}`
	_ = os.WriteFile(path, []byte(body), 0o600)
	if _, err := LoadRunConfig(path); err == nil {
		t.Fatal("expected error: duplicate source name")
	}
}
```

- [ ] **Step 2: Run — verify it fails**

Run: `go test ./cmd/nagus/ -run TestLoadRunConfig -count=1`
Expected: FAIL (`LoadRunConfig` undefined).

- [ ] **Step 3: Write `config.go`:**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// SourceConfig is one ingest source. type selects the connector builder;
// category binds it to a category's extractor (slice 1); secretRef names a
// per-source secret the chart injects as env. Connector-specific fields are
// grouped by type.
type SourceConfig struct {
	Name            string `json:"name"`
	Category        string `json:"category"`
	Type            string `json:"type"` // "ebay" | "craigslist"
	IntervalMinutes int    `json:"intervalMinutes"`
	SecretRef       string `json:"secretRef,omitempty"`

	// ebay
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`

	// craigslist
	City       string `json:"city,omitempty"`
	ClCategory string `json:"clCategory,omitempty"`

	// offline/testing
	Fixture string `json:"fixture,omitempty"`
}

// CategoryConfig is the surface/scoring config for one category.
type CategoryConfig struct {
	MinCapacityTB   float64 `json:"minCapacityTB,omitempty"`
	MinAcreageAcres float64 `json:"minAcreageAcres,omitempty"`
	MaxAcreageAcres float64 `json:"maxAcreageAcres,omitempty"`
	BudgetCents     int64   `json:"budgetCents,omitempty"`
}

// RunConfig is the whole deployment declaration: what to ingest and what to
// surface. DefaultCategory (optional) pins the read default when more than one
// category is configured.
type RunConfig struct {
	Sources         []SourceConfig            `json:"sources"`
	Categories      map[string]CategoryConfig `json:"categories"`
	DefaultCategory string                    `json:"defaultCategory,omitempty"`
}

// LoadRunConfig reads and validates config.json.
func LoadRunConfig(path string) (RunConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return RunConfig{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var c RunConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return RunConfig{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if len(c.Categories) == 0 {
		return RunConfig{}, fmt.Errorf("config %q: at least one category is required", path)
	}
	seen := map[string]bool{}
	for i, s := range c.Sources {
		if s.Name == "" {
			return RunConfig{}, fmt.Errorf("source #%d: name is required", i)
		}
		if seen[s.Name] {
			return RunConfig{}, fmt.Errorf("source %q: duplicate name", s.Name)
		}
		seen[s.Name] = true
		if !supportedCategory(s.Category) {
			return RunConfig{}, fmt.Errorf("source %q: unsupported category %q", s.Name, s.Category)
		}
		if _, ok := c.Categories[s.Category]; !ok {
			return RunConfig{}, fmt.Errorf("source %q: references category %q not in categories{}", s.Name, s.Category)
		}
	}
	if c.DefaultCategory != "" {
		if _, ok := c.Categories[c.DefaultCategory]; !ok {
			return RunConfig{}, fmt.Errorf("defaultCategory %q not in categories{}", c.DefaultCategory)
		}
	}
	return c, nil
}
```

- [ ] **Step 4: Run — verify pass**

Run: `go test ./cmd/nagus/ -run TestLoadRunConfig -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/nagus/config.go cmd/nagus/config_test.go
git commit -m "feat(nagus): config.json schema + validating loader (sources[]+categories{})"
```

---

## Task 5: Per-source / per-category builders

**Files:**
- Modify: `cmd/nagus/category.go`

- [ ] **Step 1: Add `buildIngester` and `buildSurface`** in `category.go`. They translate one `SourceConfig`/`CategoryConfig` into the new pipeline types. Reuse the existing connector builders (`buildEbayConnector`, `craigslist.NewConnector`) and the existing category dep wiring.

```go
// buildIngester builds the ingest unit for one source. StaleAfter is applied
// only for eBay content (License 8.1(b)); keyless sources retain (0).
func buildIngester(s SourceConfig, st store.Store, o categoryOpts) (*pipeline.Ingester, error) {
	var conn listing.Connector
	var stale time.Duration
	switch s.Type {
	case "ebay":
		c, err := buildEbayConnector(s.Fixture, o.ebayClientID, o.ebaySecret, orDefault(s.Query, "internal hard drive"), "EBAY_US", orInt(s.Limit, 50))
		if err != nil {
			return nil, err
		}
		conn, stale = c, 6*time.Hour
	case "craigslist":
		clCat := orDefault(s.ClCategory, "reo")
		if s.Fixture != "" {
			conn = craigslist.NewConnector(craigslist.Config{FixturePath: s.Fixture, Category: clCat})
		} else if s.City != "" {
			conn = craigslist.NewConnector(craigslist.Config{City: s.City, Category: clCat})
		} else {
			return nil, fmt.Errorf("source %q: craigslist needs city or fixture", s.Name)
		}
	default:
		return nil, fmt.Errorf("source %q: unsupported type %q", s.Name, s.Type)
	}
	return &pipeline.Ingester{
		Connector: conn, Sanitizer: sanitize.Passthrough{}, Extractor: extractorFor(s.Category, o),
		Store: st, StaleAfter: stale, Logf: o.logf,
	}, nil
}

// buildSurface builds the surface unit for one category.
func buildSurface(cat string, cc CategoryConfig, st store.Store, o categoryOpts) (*pipeline.Surface, error) {
	switch cat {
	case "hdd":
		minCap := cc.MinCapacityTB
		if minCap == 0 {
			minCap = category.DefaultMinCapacityTB
		}
		return category.NewHDDSurface(st, category.HDDDeps{Store: st, HTTPClient: o.http, MinCapacityTB: minCap, Logf: o.logf}), nil
	case "land":
		return category.NewLandSurface(st, category.LandDeps{ /* filter+valuate from cc */ }), nil
	default:
		return nil, fmt.Errorf("unsupported category %q", cat)
	}
}
```

> **Note for the implementer:** `category.NewHDDPipeline`/`NewLandPipeline` today return a `*pipeline.Pipeline`. You must add sibling constructors `NewHDDSurface`/`NewLandSurface` (returning `*pipeline.Surface`) and `NewHDDExtractor`/`extractorFor` in `internal/category`, OR expose the category's `Filter`/`Valuate`/`Extractor` so `cmd/nagus` can assemble them. Prefer adding the `*Surface`/extractor constructors in `internal/category` (keeps category internals encapsulated). Read `internal/category/hdd.go` and `land.go` first; mirror their existing dep structs. **Preserve the `-offline` path:** `buildPipeline` today sets `deps.Reference = demoReference` when `o.hddOffline`, so `NewHDDSurface`/`HDDDeps` (and `buildSurface`) must accept the same offline reference override — otherwise `nagus search -offline` (Task 8) loses its built-in reference. Add unit tests in `internal/category` for the new constructors that assert the returned `Surface`/`Ingester` behave like the old `Pipeline` on a MemoryStore fixture (including the offline-reference branch). Helpers `orDefault`, `orInt` are trivial; add them to `category.go`.

- [ ] **Step 2: Add category constructor tests** in `internal/category` (mirror existing `*_test.go`): a `NewHDDSurface` over a seeded MemoryStore returns the same ranked result the old `NewHDDPipeline(...).Surface(...)` did.

- [ ] **Step 3: Migrate the EXISTING category Pipeline tests** off `NewHDDPipeline`/`NewLandPipeline` (they must not reference `Pipeline` by the time Task 9 deletes it). These three still use the old API — rewrite them now, while both old and new constructors coexist:
  - `internal/category/hdd_test.go` `TestNewHDDPipelineSetsFreshnessWindow` — the `StaleAfter` assertion moves onto the `*pipeline.Ingester` from `buildIngester`/`NewHDDIngester` (StaleAfter lives on `Ingester`, not `Surface`). Rename to `TestNewHDDIngesterSetsFreshnessWindow`.
  - `internal/category/hdd_test.go` `TestHDDSliceEndToEnd` — seed via the `Ingester`, query via the `Surface`.
  - `internal/category/land_test.go` `TestLandPipelineStructureFirstEndToEnd` — seed via the `Ingester` (if it ingests) and query via the `Surface`.

  (If you keep `NewHDDPipeline`/`NewLandPipeline` alive until Task 9, these tests can migrate here without deleting the constructors yet; the constructors themselves are removed in Task 9 Step 2, by which point nothing references them.)

- [ ] **Step 4: Run**

Run: `go test ./internal/category/ ./cmd/nagus/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/nagus/category.go internal/category/
git commit -m "feat(nagus): per-source Ingester + per-category Surface builders; migrate category tests"
```

---

## Task 6: `server` holds collections; ALL reads dispatch by category

This is the atomic cut-over: the `server` struct loses `pipe`/`category` and gains
`surfaces`/`ingesters`. EVERY read handler that referenced `s.pipe`/`s.category`
must change in this SAME commit — `handleSearch`, `mcpSearchItems`, `handleMetrics`,
AND `handleWatches` — and `watch.EvaluateAll` changes signature with it (it now
dispatches by category over `s.surfaces`). Splitting these across commits leaves
`cmd/nagus` uncompilable, so they are one task.

**Files:**
- Modify: `cmd/nagus/serve.go`, `cmd/nagus/mcp.go`, `internal/watch/watch.go`, `cmd/nagus/serve_test.go`, `cmd/nagus/mcp_test.go`, `internal/watch/watch_test.go`

- [ ] **Step 1: Change the `server` struct** in `serve.go`:

```go
type server struct {
	ingesters       []*pipeline.Ingester
	surfaces        map[string]*pipeline.Surface
	store           store.Store
	defaultCategory string // "" when >1 category and no explicit default
	watches         watch.Config
}

// surfaceFor resolves the surface for a request's category, applying the
// absent-category rule: empty -> the single configured category if exactly one,
// else the defaultCategory, else "" (caller returns 400).
func (s *server) resolveCategory(req string) (string, bool) {
	if req != "" {
		_, ok := s.surfaces[req]
		return req, ok
	}
	if s.defaultCategory != "" {
		return s.defaultCategory, true
	}
	if len(s.surfaces) == 1 {
		for k := range s.surfaces {
			return k, true
		}
	}
	return "", false
}
```

- [ ] **Step 2: Write the failing dispatch test** in `serve_test.go`:

```go
func TestHandleSearchAbsentCategoryMultiRequires400(t *testing.T) {
	srv := &server{surfaces: map[string]*pipeline.Surface{"hdd": {}, "land": {}}, store: store.NewMemoryStore()}
	req := httptest.NewRequest(http.MethodGet, "/search", nil) // no category param
	rec := httptest.NewRecorder()
	srv.handleSearch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (ambiguous category)", rec.Code)
	}
}

func TestHandleSearchUnknownCategory400(t *testing.T) {
	srv := &server{surfaces: map[string]*pipeline.Surface{"hdd": {}}, store: store.NewMemoryStore()}
	req := httptest.NewRequest(http.MethodGet, "/search?category=ghost", nil)
	rec := httptest.NewRecorder()
	srv.handleSearch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (unknown category)", rec.Code)
	}
}
```

- [ ] **Step 3: Update `handleSearch`** (serve.go) to resolve+dispatch:

```go
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cat, ok := s.resolveCategory(r.URL.Query().Get("category"))
	if !ok {
		http.Error(w, "category required (configured: "+strings.Join(s.categoryNames(), ",")+")", http.StatusBadRequest)
		return
	}
	q := store.Query{Category: cat, Text: r.URL.Query().Get("text")}
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		q.Limit = n
	}
	res, err := s.surfaces[cat].Surface(r.Context(), q)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"matched": res.Matched, "filtered": res.Filtered, "items": scoredToRows(res)})
}
```
Add a `categoryNames()` helper (sorted keys) and import `strings`.

- [ ] **Step 4: Update `mcpSearchItems`** (mcp.go) the same way — replace `q := store.Query{Category: s.category, ...}` with the `resolveCategory` result; on unresolved category return an rpcError (invalid params). Update `mcpGetItem` only if it referenced `s.category` (it uses `s.store.Get`, so no change).

- [ ] **Step 5: Update `handleMetrics`** (serve.go) to iterate ingesters and emit the eBay budget for any eBay source:

```go
func (s *server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, ing := range s.ingesters {
		ec, ok := ing.Connector.(*ebay.Connector)
		if !ok {
			continue
		}
		st := ec.BudgetStats()
		src := ing.SourceID()
		fmt.Fprintf(w, "# HELP nagus_ebay_api_calls_budget Configured daily eBay API call budget.\n")
		fmt.Fprintf(w, "# TYPE nagus_ebay_api_calls_budget gauge\n")
		fmt.Fprintf(w, "nagus_ebay_api_calls_budget{source=%q} %d\n", src, st.Budget)
		fmt.Fprintf(w, "nagus_ebay_api_calls_used{source=%q} %d\n", src, st.Used)
		fmt.Fprintf(w, "nagus_ebay_api_calls_remaining{source=%q} %d\n", src, st.Remaining)
	}
}
```
(Emit the HELP/TYPE lines once before the loop if you prefer strict Prometheus format; per-source labels are the important change.)

- [ ] **Step 6: Update `handleWatches` + `watch.EvaluateAll` together** (they both need `s.surfaces`). In `watch.go`, change `EvaluateAll` to dispatch each watch by its category:

```go
// EvaluateAll evaluates every watch through the surface for ITS category. A watch
// naming an unconfigured category is a per-watch error (a merged deployment must
// not silently apply the wrong category's filter/valuation).
func EvaluateAll(ctx context.Context, surfaces map[string]*pipeline.Surface, cfg Config) ([]Result, error) {
	out := make([]Result, 0, len(cfg.Watches))
	for _, w := range cfg.Watches {
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
```
In `serve.go` `handleWatches`, change the call to `watch.EvaluateAll(r.Context(), s.surfaces, s.watches)`.

- [ ] **Step 7: Write the cross-category watch dispatch tests** in `watch_test.go` (a `land` watch must be evaluated by the land surface, not hdd; an unconfigured category errors per-watch):

```go
func TestEvaluateAllDispatchesByCategory(t *testing.T) {
	surfaces := map[string]*pipeline.Surface{
		"hdd":  {Store: store.NewMemoryStore(), Filter: passNone{}},           // 0 items
		"land": landSurfaceReturningOne(t),                                    // 1 item
	}
	res, err := EvaluateAll(context.Background(), surfaces, Config{Watches: []Watch{{Name: "l", Category: "land"}}})
	if err != nil {
		t.Fatalf("EvaluateAll: %v", err)
	}
	if len(res) != 1 || len(res[0].Candidates) != 1 {
		t.Fatalf("land watch not routed to land surface: %+v", res)
	}
}

func TestEvaluateAllUnknownCategoryErrors(t *testing.T) {
	surfaces := map[string]*pipeline.Surface{"hdd": {Store: store.NewMemoryStore(), Filter: passNone{}}}
	if _, err := EvaluateAll(context.Background(), surfaces, Config{Watches: []Watch{{Name: "x", Category: "ghost"}}}); err == nil {
		t.Fatal("expected per-watch unknown-category error")
	}
}
```
Build `passNone` (a `score.Filter` whose `Pass` returns false) and `landSurfaceReturningOne` (a `*pipeline.Surface` over a MemoryStore seeded with one land item and a pass-all filter). **Also update the EXISTING `TestEvaluateAll`** (currently calls `EvaluateAll(ctx, p, cfg)` with a `*Pipeline`) to the new `surfaces` map signature, and update the `mkPipeline` helper's callers here to hand a `map[string]*pipeline.Surface` where these tests need it. (The single-watch `Evaluate` tests still pass a `*Pipeline` — legal until Task 9, since `*Pipeline` satisfies `Surfacer`.)

- [ ] **Step 8: Update `serve_test.go` / `mcp_test.go` construction.** Replace every `&server{pipe: p, store: st, category: "hdd"}` with:

```go
&server{surfaces: map[string]*pipeline.Surface{"hdd": hddSurface}, store: st, defaultCategory: "hdd"}
```
where `hddSurface` is built via the category constructor from Task 5 (or a hand-built `&pipeline.Surface{Store: st, Filter: ...}`). Keep existing assertions. **Note:** `serve_test.go` currently seeds the store by `NewHDDPipeline(...).Ingest(...)` before searching — a `*Surface` does NOT populate the store, so keep seeding via an `Ingester` (or direct `store.Put`) or the search assertions will silently return empty.

- [ ] **Step 9: Build `server` from config in `runServe`** (serve.go). Replace the single-pipeline construction (lines ~222-240) with: load `config.json` (flag `-config` / `NAGUS_CONFIG`); if absent, synthesize a one-source `RunConfig` from the existing env/flags (back-compat). Then:

```go
cfg, err := resolveRunConfig(configPath, /* legacy flags */)
if err != nil { return err }
surfaces := map[string]*pipeline.Surface{}
for cat, cc := range cfg.Categories {
	sf, err := buildSurface(cat, cc, st, opts)
	if err != nil { return err }
	surfaces[cat] = sf
}
var ingesters []*pipeline.Ingester
for _, sc := range cfg.Sources {
	ing, err := buildIngester(sc, st, opts)
	if err != nil { return err }
	ingesters = append(ingesters, ing)
}
def := cfg.DefaultCategory
if def == "" && len(surfaces) == 1 { for k := range surfaces { def = k } }
srv := &server{ingesters: ingesters, surfaces: surfaces, store: st, defaultCategory: def, watches: watches}
```
Add `resolveRunConfig` (loads the file or builds the legacy single-source config). **Delete the old `go runIngestLoop(ctx, p, *interval)` call line (serve.go ~246)** in this step — its operand `p` is gone; per-source goroutine startup is (re)introduced in Task 7. Leave the `runIngestLoop`/`ingestOnce` function definitions in place for now (Task 7 replaces them); they are unreferenced between Tasks 6 and 7 but that only trips staticcheck, which does not run until Task 9 (by which point they are deleted).

- [ ] **Step 10: Run the full gate** (this task must compile cmd/nagus AND internal/watch together — the whole point of the atomic cut-over)

Run: `go vet ./... && go test ./cmd/nagus/ ./internal/watch/ -count=1 -race`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add cmd/nagus/serve.go cmd/nagus/mcp.go internal/watch/watch.go internal/watch/watch_test.go cmd/nagus/serve_test.go cmd/nagus/mcp_test.go
git commit -m "feat(nagus): server holds ingesters+surfaces; all reads (search/mcp/watches) dispatch by category"
```

---

## Task 7: Per-source ingest loop with failure isolation

**Files:**
- Modify: `cmd/nagus/serve.go`

- [ ] **Step 1: Write the failing isolation test** in `serve_test.go`:

```go
func TestRunIngestLoopsIsolateFailures(t *testing.T) {
	st := store.NewMemoryStore()
	good := &pipeline.Ingester{Connector: fakeConn{id: "good", raws: []listing.Raw{ /* one valid hdd raw */ }},
		Sanitizer: sanitize.Passthrough{}, Extractor: /* hdd extractor */, Store: st}
	bad := &pipeline.Ingester{Connector: fakeConn{id: "bad", err: errors.New("boom")},
		Sanitizer: sanitize.Passthrough{}, Extractor: /* hdd extractor */, Store: st}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// run each once synchronously via the extracted single-shot helper:
	ingestOnceSource(ctx, good)
	ingestOnceSource(ctx, bad) // must not panic / must not affect good
	if n, _ := st.Search(ctx, store.Query{Category: "hdd"}); len(n) == 0 {
		t.Fatal("good source did not store despite bad source failing")
	}
}
```
(You will define a small `fakeConn` test double in the cmd/nagus test package, or reuse an existing one.)

- [ ] **Step 2: Rework the ingest loop** in serve.go to one goroutine per source, each on its own interval:

```go
func (s *server) startIngest(ctx context.Context, def time.Duration, perSource map[string]time.Duration) {
	for _, ing := range s.ingesters {
		iv := def
		if d, ok := perSource[ing.SourceID()]; ok && d > 0 {
			iv = d
		}
		if iv <= 0 {
			continue // ingest disabled for this source
		}
		go runSourceIngestLoop(ctx, ing, iv)
	}
}

func runSourceIngestLoop(ctx context.Context, ing *pipeline.Ingester, interval time.Duration) {
	ingestOnceSource(ctx, ing)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ingestOnceSource(ctx, ing)
		}
	}
}

func ingestOnceSource(ctx context.Context, ing *pipeline.Ingester) {
	res, err := ing.Ingest(ctx)
	if err != nil {
		if errors.Is(err, ebay.ErrBudgetExhausted) {
			fmt.Fprintf(os.Stderr, "nagus serve: source %s eBay budget exhausted; backing off\n", ing.SourceID())
			return
		}
		fmt.Fprintf(os.Stderr, "nagus serve: source %s ingest error: %v\n", ing.SourceID(), err)
		return
	}
	fmt.Fprintf(os.Stderr, "nagus serve: source %s fetched=%d stored=%d purged=%d skipped=%d\n",
		ing.SourceID(), res.Fetched, res.Stored, res.Purged, len(res.Skips))
}
```
Populate `perSource` intervals from each `SourceConfig.IntervalMinutes` (thread the map through from `runServe`). Delete the old `runIngestLoop`/`ingestOnce` (they operated on a single `*Pipeline`).

- [ ] **Step 3: Run**

Run: `go test ./cmd/nagus/ -run TestRunIngestLoops -count=1 -race`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/nagus/serve.go cmd/nagus/serve_test.go
git commit -m "feat(nagus): one ingest goroutine per source, per-source interval + failure isolation"
```

---

## Task 8: Migrate the CLI subcommands + watch test helper off `Pipeline`

The `nagus ingest` and `nagus search` subcommands in `main.go`, and the
`mkPipeline` helper in `watch_test.go`, still use the `Pipeline` API. They must be
moved to `Ingester`/`Surface` BEFORE Task 9 deletes `Pipeline`, or Task 9 will not
compile.

**Files:**
- Modify: `cmd/nagus/main.go` (`runIngest`, `runSearch`), `internal/watch/watch_test.go` (`mkPipeline`)

- [ ] **Step 1: Read `main.go`** `runIngest` (~line 105) and `runSearch` (~line 197). Note they call `buildSourceConnector` + `buildPipeline` and then `p.Ingest` / `p.Surface`.

- [ ] **Step 2: Migrate `runIngest`** to build a single `*pipeline.Ingester` via `buildIngester` (construct a one-off `SourceConfig` from the ingest subcommand's flags) and call `ing.Ingest(ctx)`. Preserve the existing output/summary. Keep the `-offline`/fixture flags working (map them onto `SourceConfig.Fixture`).

- [ ] **Step 3: Migrate `runSearch`** to build a single `*pipeline.Surface` via `buildSurface` (construct a `CategoryConfig` from the search subcommand's flags) and call `sf.Surface(ctx, q)`. **Thread the `-offline` reference through** — `buildSurface`/`NewHDDSurface` must accept the offline reference override (added in Task 5) so `search -offline` keeps scoring against the built-in demo reference exactly as `buildPipeline` did.

- [ ] **Step 4: Migrate `mkPipeline`** in `watch_test.go` to return a `*pipeline.Surface` (rename to `mkSurface`), building it directly (`&pipeline.Surface{Store: st, Filter: ..., Valuate: ...}`) so the single-watch `Evaluate` tests no longer depend on `*Pipeline`.

- [ ] **Step 5: Run**

Run: `go vet ./... && go test ./cmd/nagus/ ./internal/watch/ -count=1 -race`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/nagus/main.go internal/watch/watch_test.go
git commit -m "refactor(nagus): migrate CLI ingest/search + watch tests off Pipeline"
```

---

## Task 9: Remove the dead `Pipeline` type

**Files:**
- Modify: `internal/pipeline/pipeline.go`, `internal/pipeline/pipeline_test.go`, `cmd/nagus/category.go`, `internal/category/*`

- [ ] **Step 1: Delete `Pipeline`** and its delegating `Ingest`/`Surface` methods from `pipeline.go`, keeping the SHARED types (`IngestResult`, `Skip`, `Scored`, `SurfaceResult`) and the package doc. Delete the now-redundant ingest/surface tests from `pipeline_test.go` (their behavior is covered by `ingester_test.go`/`surface_test.go`); keep the shared test fakes (`fakeConnector`, `fakeExtractor`, `raw`) — move them into `ingester_test.go` if `pipeline_test.go` becomes empty.
- [ ] **Step 2: Delete `buildPipeline` and `buildSourceConnector`** from `cmd/nagus/category.go` (replaced by `buildIngester`/`buildSurface`). Delete `NewHDDPipeline`/`NewLandPipeline` from `internal/category`. Their only callers were the CLI (migrated in Task 8) and the category tests (migrated in Task 5), so the grep in Step 3 must now be clean — if it is not, the offending file was missed by Tasks 5/8; migrate it before deleting the constructors.
- [ ] **Step 3: Grep for stragglers**

Run: `grep -rn "pipeline.Pipeline\|buildPipeline\|NewHDDPipeline\|NewLandPipeline\|buildSourceConnector" --include=*.go .`
Expected: no matches (or only comments).

- [ ] **Step 4: Full gate**

Run: `go vet ./... && go test ./... -count=1 -race && staticcheck ./...`
Expected: PASS, no findings.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "refactor(pipeline): remove welded Pipeline; Ingester+Surface are the units"
```

---

## Task 10: Chart — config.json + merged release

**Files:**
- Modify: `charts/nagus/values.yaml`
- Create: `charts/nagus/templates/configmap-config.yaml`
- Modify: `charts/nagus/templates/deployment.yaml`

- [ ] **Step 1: Read the current chart** (`values.yaml`, `templates/deployment.yaml`) so the edits follow existing naming/label conventions.

- [ ] **Step 2: Add `sources`/`categories` to `values.yaml`** (keep old keys until gitops is migrated, or remove if you also do Task 11 in the same MR):

```yaml
# Multi-source: one deployment ingests N sources, serves M categories.
sources:
  - name: ebay
    category: hdd
    type: ebay
    query: "internal hard drive"
    intervalMinutes: 30
    secretRef: ebay
  - name: cl-seattle
    category: land
    type: craigslist
    city: seattle
    clCategory: rea
    intervalMinutes: 60
categories:
  hdd:  { minCapacityTB: 8 }
  land: { minAcreageAcres: 1, budgetCents: 0 }
defaultCategory: ""   # required only when >1 category and callers omit ?category=

# secretRef -> ExternalSecret mapping (Vault keys under eso/nagus/sources).
sourceSecrets:
  ebay:
    NAGUS_EBAY_CLIENT_ID:     ebay_client_id
    NAGUS_EBAY_CLIENT_SECRET: ebay_client_secret
  rentcast:
    NAGUS_RENTCAST_KEY:       rentcast_key
```

- [ ] **Step 3: Create `templates/configmap-config.yaml`** rendering `config.json`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "nagus.fullname" . }}-config
  labels: {{- include "nagus.labels" . | nindent 4 }}
data:
  config.json: |
    {{ toJson (dict "sources" .Values.sources "categories" .Values.categories "defaultCategory" .Values.defaultCategory) | nindent 4 }}
```

- [ ] **Step 4: Mount it and set `NAGUS_CONFIG`** in `templates/deployment.yaml`: add a `configMap` volume + `volumeMount` at `/etc/nagus/config.json`, and env `NAGUS_CONFIG=/etc/nagus/config.json`. Wire each `sourceSecrets` entry as an ExternalSecret→secret→`envFrom`/`env` (follow the existing eBay externalSecret block). Remove the single-category `NAGUS_CATEGORY` wiring.

- [ ] **Step 5: Verify the chart renders**

Run: `helm template charts/nagus | grep -A3 config.json` and `helm template charts/nagus > /dev/null && echo OK`
Expected: `config.json` present with sources/categories; template renders without error.

- [ ] **Step 6: Commit**

```bash
git add charts/nagus/values.yaml charts/nagus/templates/configmap-config.yaml charts/nagus/templates/deployment.yaml
git commit -m "feat(chart): render config.json (sources[]+categories{}); mount + per-source secrets"
```

---

## Task 11: Gitops — collapse the two HelmReleases (DEPLOY)

**Repo:** `steve/gitops` (NOT this repo). Done after the image with these changes is built by GitLab CI (tag = 8-char SHA of the merged branch on `main`).

- [ ] **Step 1:** In `clusters/orac/apps/nagus/`, edit `helmrelease-nagus.yaml` `values` to carry the full `sources`/`categories` (both hdd + land) and bump `image.tag` to the new SHA.
- [ ] **Step 2:** Delete `helmrelease-nagus-land.yaml` and its entry from `kustomization.yaml`.
- [ ] **Step 3:** Validate before commit: `kustomize build clusters/orac/apps/nagus | kubeconform -strict` (or the repo's existing validation) and `helm template` the merged values.
- [ ] **Step 4:** Commit + push gitops `main`; `flux reconcile kustomization apps -n flux-system --with-source`.
- [ ] **Step 5:** Verify: one `nagus` deployment Ready; `kubectl -n nagus get deploy` shows no `nagus-land`; `/healthz`, `/search?category=hdd`, `/search?category=land` all respond; `/metrics` shows per-source eBay budget. Live eBay/Craigslist failures (nagus-9nx / nagus-hh5) remain expected and separate.

> Update `bd`/beads and the go-live memory after cutover. Do not delete the old Longhorn PVC until the merged release is confirmed healthy.

---

## Done criteria

- `go vet ./...`, `go test ./... -count=1 -race`, `staticcheck ./...` all clean.
- One deployment ingests eBay (hdd) + Craigslist (land) on independent intervals with per-source isolation; `search_items`/`/search`/`/watches` dispatch by category; watches span categories correctly.
- `pipeline.Pipeline` is gone; `Ingester` + `Surface` are the units.
- Chart renders `config.json`; gitops runs a single merged HelmRelease.
- No offer store, no inquiries, no dashboard (later slices) — scope held.
