// Command nagus drives the acquisition/watch spine from the command line.
//
// Subcommands:
//
//	nagus version
//	nagus ingest -category hdd -db nagus.db [-ebay-fixture FILE | -client-id ... -client-secret ...] [-query "..."]
//	nagus search -category hdd -db nagus.db [-text ...] [-min-capacity 6] [-limit 20] [-offline] [-json]
//
// ingest runs the front half of the spine (connector -> sanitize -> extract ->
// store); search runs the back half (query -> hard-filter -> valuation enrich ->
// score -> rank) and prints ranked candidates. search is READ-ONLY: it surfaces,
// it never acts (eyes, not hands). This is the runnable proof of the HDD
// vertical slice; the same store persists between the two invocations.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/leftathome/nagus/internal/category"
	"github.com/leftathome/nagus/internal/connector/ebay"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
	"github.com/leftathome/nagus/internal/store/sqlitestore"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "-version", "--version":
		fmt.Println(version)
	case "ingest":
		if err := runIngest(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "nagus ingest:", err)
			os.Exit(1)
		}
	case "search":
		if err := runSearch(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "nagus search:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "nagus: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `nagus -- acquisition/watch spine

usage:
  nagus version
  nagus ingest -category hdd -db nagus.db (-ebay-fixture FILE | -client-id ID -client-secret SECRET) [-query "internal hard drive"] [-limit 50]
  nagus search -category hdd -db nagus.db [-text STR] [-min-capacity 6] [-limit 20] [-offline] [-json]

ingest collects listings and stores normalized items; search surfaces ranked
$/TB deals from the store (read-only). Use -offline on search to score against a
built-in reference instead of the live feed (for the fixture-driven proof).
`)
}

// openStore opens the sqlite-backed store at dsn (a file path, or ":memory:").
func openStore(dsn string) (store.Store, error) {
	return sqlitestore.New(dsn)
}

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	cat := fs.String("category", "hdd", "category bundle to ingest (v1: hdd)")
	db := fs.String("db", "nagus.db", "sqlite store path (or :memory:)")
	fixture := fs.String("ebay-fixture", "", "path to an eBay Browse JSON fixture (offline ingest; skips the network)")
	clientID := fs.String("client-id", "", "eBay OAuth client id (live ingest; prefer env/Vault injection)")
	clientSecret := fs.String("client-secret", "", "eBay OAuth client secret (live ingest)")
	query := fs.String("query", "internal hard drive", "eBay search query (live ingest)")
	marketplace := fs.String("marketplace", "EBAY_US", "eBay marketplace id (live ingest)")
	limit := fs.Int("limit", 50, "max listings to fetch (live ingest)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cat != "hdd" {
		return fmt.Errorf("only -category hdd is wired in v1 (got %q)", *cat)
	}

	conn, err := buildEbayConnector(*fixture, *clientID, *clientSecret, *query, *marketplace, *limit)
	if err != nil {
		return err
	}

	st, err := openStore(*db)
	if err != nil {
		return fmt.Errorf("open store %q: %w", *db, err)
	}
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "  "+format+"\n", a...) }
	p := category.NewHDDPipeline(conn, category.HDDDeps{Store: st, Logf: logf})

	res, err := p.Ingest(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("ingest[%s]: fetched=%d stored=%d skipped=%d (db=%s)\n",
		*cat, res.Fetched, res.Stored, len(res.Skips), *db)
	for _, s := range res.Skips {
		fmt.Printf("  skip %s at %s: %s\n", s.SourceKey, s.Stage, s.Reason)
	}
	return nil
}

func buildEbayConnector(fixture, clientID, clientSecret, query, marketplace string, limit int) (listing.Connector, error) {
	if fixture != "" {
		return ebay.NewConnector(ebay.Config{FixturePath: fixture}), nil
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("live ingest needs -client-id and -client-secret (or use -ebay-fixture for offline)")
	}
	return ebay.NewConnector(ebay.Config{
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		MarketplaceID: marketplace,
		Query:         query,
		Limit:         limit,
	}), nil
}

// demoReference is the built-in offline reference (cents/TB by condition) used
// by `search -offline` so the fixture-driven proof scores without the network.
// Production scoring uses the live category.DefaultReferenceProductsURL feed.
var demoReference = category.StaticReference{CentsPerTB: map[string]int64{
	"new":    1900,
	"refurb": 1400,
	"used":   1150,
}}

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	cat := fs.String("category", "hdd", "category to search (v1: hdd)")
	db := fs.String("db", "nagus.db", "sqlite store path (or :memory:)")
	text := fs.String("text", "", "case-insensitive text match over title/tokens")
	minCap := fs.Float64("min-capacity", category.DefaultMinCapacityTB, "hard-filter capacity floor in TB")
	limit := fs.Int("limit", 20, "max items to surface")
	offline := fs.Bool("offline", false, "score against the built-in demo reference instead of the live feed")
	asJSON := fs.Bool("json", false, "emit results as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cat != "hdd" {
		return fmt.Errorf("only -category hdd is wired in v1 (got %q)", *cat)
	}

	st, err := openStore(*db)
	if err != nil {
		return fmt.Errorf("open store %q: %w", *db, err)
	}
	deps := category.HDDDeps{Store: st, MinCapacityTB: *minCap}
	if *offline {
		deps.Reference = demoReference
	}
	p := category.NewHDDPipeline(nil, deps)

	q := store.Query{Category: "hdd", Text: *text, Limit: *limit}
	res, err := p.Surface(context.Background(), q)
	if err != nil {
		return err
	}
	if *asJSON {
		return emitJSON(res)
	}
	emitTable(res)
	return nil
}

func emitTable(res pipeline.SurfaceResult) {
	fmt.Printf("search[hdd]: matched=%d survived-filter=%d\n", res.Matched, res.Filtered)
	if len(res.Items) == 0 {
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "#\tVERDICT\tSCORE\t$/TB\tPRICE\tCOND\tCAP_TB\tTITLE")
	for i, sc := range res.Items {
		fmt.Fprintf(w, "%d\t%s\t%.0f\t%s\t%s\t%s\t%s\t%s\n",
			i+1,
			sc.Signal.Verdict,
			sc.Score.Value,
			dollarsPerTB(sc.Item.PriceCents, capacityTB(sc.Item)),
			dollars(sc.Item.PriceCents),
			orDash(sc.Item.Condition),
			orDash(sc.Item.Attributes["capacity_tb"]),
			truncate(sc.Item.Title, 52),
		)
	}
	w.Flush()
}

func emitJSON(res pipeline.SurfaceResult) error {
	type row struct {
		Rank       int     `json:"rank"`
		ID         string  `json:"id"`
		Verdict    string  `json:"verdict"`
		Score      float64 `json:"score"`
		Rationale  string  `json:"rationale"`
		PriceCents int64   `json:"price_cents"`
		CapacityTB string  `json:"capacity_tb"`
		Condition  string  `json:"condition"`
		Title      string  `json:"title"`
		URL        string  `json:"source_url"`
	}
	out := make([]row, 0, len(res.Items))
	for i, sc := range res.Items {
		out = append(out, row{
			Rank: i + 1, ID: sc.Item.ID, Verdict: sc.Signal.Verdict,
			Score: sc.Score.Value, Rationale: sc.Score.Rationale,
			PriceCents: sc.Item.PriceCents, CapacityTB: sc.Item.Attributes["capacity_tb"],
			Condition: sc.Item.Condition, Title: sc.Item.Title, URL: sc.Item.SourceURL,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// --- small display helpers ---

func capacityTB(it item.Item) string { return it.Attributes["capacity_tb"] }

func dollars(cents int64) string {
	if cents <= 0 {
		return "-"
	}
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

// dollarsPerTB formats price/capacity as $/TB for display. It is a presentation
// helper only; the scored $/TB verdict comes from the valuation adapter.
func dollarsPerTB(cents int64, capStr string) string {
	if cents <= 0 || capStr == "" {
		return "-"
	}
	capTB, err := strconv.ParseFloat(capStr, 64)
	if err != nil || capTB <= 0 {
		return "-"
	}
	return fmt.Sprintf("$%.2f", float64(cents)/100.0/capTB)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
