package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/leftathome/nagus/internal/category"
	"github.com/leftathome/nagus/internal/connector/ebay"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
	"github.com/leftathome/nagus/internal/watch"
)

// server holds the wired pipeline behind the read-only HTTP surface. This is
// the deployable workload: one process that (optionally) ingests on an interval
// and serves search_items / get_item. It is READ-ONLY over the store -- it
// surfaces candidates, it never acts (eyes, not hands; design section 11).
type server struct {
	ingesters       []*pipeline.Ingester
	surfaces        map[string]*pipeline.Surface
	store           store.Store
	defaultCategory string // "" when >1 category and no explicit default
	watches         watch.Config
}

// resolveCategory applies the absent-category rule: empty -> the single
// configured category if exactly one, else defaultCategory, else ("",false).
func (s *server) resolveCategory(req string) (string, bool) {
	if req != "" {
		_, ok := s.surfaces[req]
		return req, ok
	}
	if s.defaultCategory != "" {
		if _, ok := s.surfaces[s.defaultCategory]; ok {
			return s.defaultCategory, true
		}
	}
	if len(s.surfaces) == 1 {
		for k := range s.surfaces {
			return k, true
		}
	}
	return "", false
}

// categoryNames returns the configured category names, sorted (for error text).
func (s *server) categoryNames() []string {
	names := make([]string, 0, len(s.surfaces))
	for k := range s.surfaces {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/item", s.handleItem)
	mux.HandleFunc("/watches", s.handleWatches)
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/metrics", s.handleMetrics)
	return mux
}

// handleMetrics emits Prometheus-text metrics. Currently it reports the eBay API
// call budget (License 2.4: we track usage against the ~5k/day production cap and
// never circumvent it). When the source is not the eBay connector (e.g. land /
// Craigslist), there is no budget to report and the body is empty.
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

// searchRow is the typed JSON shape returned by /search (search_items). It is
// deliberately typed fields plus one quoted free-text title (safe as data).
type searchRow struct {
	Rank       int     `json:"rank"`
	ID         string  `json:"id"`
	Verdict    string  `json:"verdict"`
	Score      float64 `json:"score"`
	Rationale  string  `json:"rationale"`
	PriceCents int64   `json:"price_cents"`
	Currency   string  `json:"currency"`
	CapacityTB string  `json:"capacity_tb"`
	Condition  string  `json:"condition"`
	Title      string  `json:"title"`
	SourceURL  string  `json:"source_url"`
}

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
	writeJSON(w, map[string]any{
		"matched":  res.Matched,
		"filtered": res.Filtered,
		"items":    scoredToRows(res),
	})
}

func (s *server) handleItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	it, ok, err := s.store.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, it)
}

// handleWatches evaluates the configured watches and returns, per watch, the
// ranked candidates (quiet inbox) and strong matches (ping) plus the opaque
// audience tag. Read-only: openclaw's delivery cron polls this and routes via
// the household/audience resolver -- nagus reports, it does not deliver.
func (s *server) handleWatches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	results, err := watch.EvaluateAll(r.Context(), s.surfaces, s.watches)
	if err != nil {
		http.Error(w, "watch evaluation failed", http.StatusInternalServerError)
		return
	}
	type watchOut struct {
		Name           string      `json:"name"`
		Audience       string      `json:"audience,omitempty"`
		CandidateCount int         `json:"candidate_count"`
		StrongCount    int         `json:"strong_count"`
		Candidates     []searchRow `json:"candidates"`
		Strong         []searchRow `json:"strong"`
	}
	out := make([]watchOut, 0, len(results))
	for _, res := range results {
		out = append(out, watchOut{
			Name: res.Watch.Name, Audience: res.Watch.Audience,
			CandidateCount: len(res.Candidates), StrongCount: len(res.Strong),
			Candidates: scoredItemsToRows(res.Candidates),
			Strong:     scoredItemsToRows(res.Strong),
		})
	}
	writeJSON(w, map[string]any{"watches": out})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cat := fs.String("category", envOr("NAGUS_CATEGORY", "hdd"), "category bundle to serve (v1: hdd)")
	configPath := fs.String("config", envOr("NAGUS_CONFIG", ""), "path to a multi-source run config (config.json); overrides the legacy single-source flags")
	sflags := registerStoreFlags(fs)
	listen := fs.String("listen", envOr("NAGUS_LISTEN", ":8080"), "HTTP listen address")
	interval := fs.Duration("ingest-interval", envDuration("NAGUS_INGEST_INTERVAL", 0), "in-process ingest interval (0 disables scheduled ingest)")
	minCap := fs.Float64("min-capacity", envFloat("NAGUS_MIN_CAPACITY", category.DefaultMinCapacityTB), "hard-filter capacity floor in TB")
	offline := fs.Bool("offline", envBool("NAGUS_OFFLINE"), "score against the built-in demo reference instead of the live feed")
	fixture := fs.String("ebay-fixture", envOr("NAGUS_EBAY_FIXTURE", ""), "eBay fixture path for offline ingest (skips the network)")
	clientID := fs.String("client-id", envOr("NAGUS_EBAY_CLIENT_ID", ""), "eBay OAuth client id (live ingest)")
	clientSecret := fs.String("client-secret", envOr("NAGUS_EBAY_CLIENT_SECRET", ""), "eBay OAuth client secret (live ingest)")
	query := fs.String("query", envOr("NAGUS_EBAY_QUERY", "internal hard drive"), "eBay search query (live ingest)")
	watchesPath := fs.String("watches", envOr("NAGUS_WATCHES", ""), "path to a JSON watches config (enables /watches for the delivery cron)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var watches watch.Config
	if *watchesPath != "" {
		loaded, lerr := watch.LoadConfig(*watchesPath)
		if lerr != nil {
			return lerr
		}
		watches = loaded
	}

	st, closeSt, err := sflags.open(context.Background())
	if err != nil {
		return err
	}
	defer closeSt()

	logf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, "  "+f+"\n", a...) }
	opts := categoryOptsFromEnv(*offline, http.DefaultClient, logf)
	opts.hddMinCapacity = *minCap
	// Explicit connector flags override the env-seeded defaults (legacy path).
	opts.ebayClientID = orDefault(*clientID, opts.ebayClientID)
	opts.ebaySecret = orDefault(*clientSecret, opts.ebaySecret)

	legacy := legacySource{
		interval:    *interval,
		ebayQuery:   *query,
		ebayFixture: *fixture,
		clCity:      envOr("NAGUS_CL_CITY", ""),
		clCategory:  envOr("NAGUS_CL_CATEGORY", "reo"),
		clFixture:   envOr("NAGUS_CL_FIXTURE", ""),
	}
	cfg, err := resolveRunConfig(*configPath, *cat, opts, legacy)
	if err != nil {
		return err
	}

	surfaces := make(map[string]*pipeline.Surface, len(cfg.Categories))
	for name, cc := range cfg.Categories {
		sf, serr := buildSurface(name, cc, st, opts)
		if serr != nil {
			return serr
		}
		surfaces[name] = sf
	}
	ingesters := make([]*pipeline.Ingester, 0, len(cfg.Sources))
	for _, src := range cfg.Sources {
		ing, ierr := buildIngester(src, st, opts)
		if ierr != nil {
			return ierr
		}
		ingesters = append(ingesters, ing)
	}
	def := cfg.DefaultCategory
	if def == "" && len(cfg.Categories) == 1 {
		for name := range cfg.Categories {
			def = name
		}
	}
	srv := &server{ingesters: ingesters, surfaces: surfaces, store: st, defaultCategory: def, watches: watches}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "nagus serve: categories=%d sources=%d backend=%s listen=%s\n", len(surfaces), len(ingesters), *sflags.backend, *listen)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "nagus serve: shutdown signal, draining...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errc:
		return err
	}
}

// legacySource carries the pre-config CLI's single-source connector settings, so
// resolveRunConfig can synthesize a one-category/one-source RunConfig when no
// config.json is supplied (back-compat with the old flags).
type legacySource struct {
	interval    time.Duration
	ebayQuery   string
	ebayFixture string
	clCity      string
	clCategory  string
	clFixture   string
}

// resolveRunConfig loads config.json when configPath != "", else synthesizes a
// single-source RunConfig from the legacy flags (back-compat with the old CLI).
// The synthesized config has exactly one category (cat, configured from o) and,
// only when scheduled ingest is enabled (legacy.interval > 0), one source of the
// type matching that category. With interval == 0 it is surface-only (no source).
func resolveRunConfig(configPath, cat string, o categoryOpts, legacy legacySource) (RunConfig, error) {
	if configPath != "" {
		return LoadRunConfig(configPath)
	}
	if !supportedCategory(cat) {
		return RunConfig{}, fmt.Errorf("unsupported category %q (want hdd or land)", cat)
	}
	cc := CategoryConfig{}
	switch cat {
	case "hdd":
		cc.MinCapacityTB = o.hddMinCapacity
	case "land":
		cc.MinAcreageAcres = o.landMinAcreage
		cc.MaxAcreageAcres = o.landMaxAcreage
		cc.BudgetCents = o.landBudgetCents
	}
	rc := RunConfig{
		Categories:      map[string]CategoryConfig{cat: cc},
		DefaultCategory: cat,
	}
	if legacy.interval > 0 {
		src := SourceConfig{
			Name:            cat + "-legacy",
			Category:        cat,
			IntervalMinutes: int(legacy.interval / time.Minute),
		}
		switch cat {
		case "hdd":
			src.Type = "ebay"
			src.Query = legacy.ebayQuery
			src.Fixture = legacy.ebayFixture
		case "land":
			src.Type = "craigslist"
			src.City = legacy.clCity
			src.ClCategory = legacy.clCategory
			src.Fixture = legacy.clFixture
		}
		rc.Sources = []SourceConfig{src}
	}
	return rc, nil
}

// runIngestLoop runs Ingest immediately, then on every tick, until ctx is done.
// An ingest error is logged and the loop continues (a transient source failure
// must not take down the surface).
func runIngestLoop(ctx context.Context, p *pipeline.Pipeline, interval time.Duration) {
	ingestOnce(ctx, p)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ingestOnce(ctx, p)
		}
	}
}

func ingestOnce(ctx context.Context, p *pipeline.Pipeline) {
	res, err := p.Ingest(ctx)
	if err != nil {
		if errors.Is(err, ebay.ErrBudgetExhausted) {
			// Not a failure: the daily eBay API budget is spent. Back off and
			// wait for the next window rather than alarming or circumventing.
			fmt.Fprintf(os.Stderr, "nagus serve: eBay API budget exhausted; backing off until next window\n")
			return
		}
		fmt.Fprintf(os.Stderr, "nagus serve: ingest error: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "nagus serve: ingest fetched=%d stored=%d skipped=%d\n", res.Fetched, res.Stored, len(res.Skips))
}

// --- env helpers (flag defaults seed from env; explicit flags override) ---

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string) bool {
	v, _ := strconv.ParseBool(os.Getenv(key))
	return v
}

func envFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
