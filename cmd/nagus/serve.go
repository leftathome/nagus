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
	"strconv"
	"syscall"
	"time"

	"github.com/leftathome/nagus/internal/category"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
	"github.com/leftathome/nagus/internal/watch"
)

// server holds the wired pipeline behind the read-only HTTP surface. This is
// the deployable workload: one process that (optionally) ingests on an interval
// and serves search_items / get_item. It is READ-ONLY over the store -- it
// surfaces candidates, it never acts (eyes, not hands; design section 11).
type server struct {
	pipe     *pipeline.Pipeline
	store    store.Store
	category string
	watches  watch.Config
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
	return mux
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
	q := store.Query{Category: s.category}
	if c := r.URL.Query().Get("category"); c != "" {
		q.Category = c
	}
	q.Text = r.URL.Query().Get("text")
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		q.Limit = n
	}
	res, err := s.pipe.Surface(r.Context(), q)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	rows := scoredToRows(res)
	writeJSON(w, map[string]any{
		"matched":  res.Matched,
		"filtered": res.Filtered,
		"items":    rows,
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
	results, err := watch.EvaluateAll(r.Context(), s.pipe, s.watches)
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
	if !supportedCategory(*cat) {
		return fmt.Errorf("unsupported category %q (want hdd or land)", *cat)
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

	// A connector is only needed if scheduled ingest is enabled.
	var conn listing.Connector
	if *interval > 0 {
		conn, err = buildSourceConnector(*cat, sourceParams{
			ebayFixture: *fixture, ebayClientID: *clientID, ebaySecret: *clientSecret, ebayQuery: *query, ebayLimit: 50,
			clFixture: envOr("NAGUS_CL_FIXTURE", ""), clCity: envOr("NAGUS_CL_CITY", ""), clCategory: envOr("NAGUS_CL_CATEGORY", "reo"),
		})
		if err != nil {
			return fmt.Errorf("ingest enabled but no source: %w", err)
		}
	}
	p, err := buildPipeline(*cat, conn, st, opts)
	if err != nil {
		return err
	}
	srv := &server{pipe: p, store: st, category: *cat, watches: watches}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *interval > 0 {
		go runIngestLoop(ctx, p, *interval)
	}

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "nagus serve: category=%s backend=%s listen=%s ingest-interval=%s\n", *cat, *sflags.backend, *listen, interval.String())
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
