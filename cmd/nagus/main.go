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
	"net/http"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/leftathome/nagus/internal/category"
	"github.com/leftathome/nagus/internal/connector/ebay"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
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
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "nagus serve:", err)
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
  nagus ingest -category hdd  ... (-ebay-fixture FILE | -client-id ID -client-secret SECRET) [-query ...] [-limit 50]
  nagus search -category hdd|land -db nagus.db [-text STR] [-limit 20] [-min-capacity 6] [-offline] [-json]
  nagus serve  -category hdd|land -db /data/nagus.db [-listen :8080] [-ingest-interval 30m] [-offline]

Categories: hdd ($/TB deal-watch, eBay), land (structure-first + free gov geo
enrichment; NAGUS_LAND_* and NAGUS_RENTCAST_KEY env), and wine (critic-score
quality + hedonic value + per-destination ship-legality; NAGUS_WINE_* incl.
NAGUS_WINE_SHIP_TO, NAGUS_WINE_SHIP_RULES, and NAGUS_LWIN_CSV env). ingest
collects + stores; search/serve surface ranked candidates read-only (eyes,
not hands).

land acquisition (zillapi) is anchored on a bounding box, which these legacy
single-source flags cannot express, so "ingest -category land" errors and directs
you to the multi-source config path: declare a zillapi source in a config.json and
run "nagus serve -config FILE". land still surfaces normally from stored items.
`)
}

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	cat := fs.String("category", "hdd", "category bundle to ingest (hdd|land|wine)")
	sflags := registerStoreFlags(fs)
	fixture := fs.String("ebay-fixture", "", "eBay Browse JSON fixture (offline hdd ingest)")
	clientID := fs.String("client-id", "", "eBay OAuth client id (live hdd ingest)")
	clientSecret := fs.String("client-secret", "", "eBay OAuth client secret (live hdd ingest)")
	query := fs.String("query", "internal hard drive", "eBay search query (live hdd ingest)")
	limit := fs.Int("limit", 50, "eBay max listings (live hdd ingest)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !supportedCategory(*cat) {
		return fmt.Errorf("unsupported category %q (want hdd or land)", *cat)
	}
	// land ingest is only reachable through the multi-source config path. Its
	// connector (zillapi) is anchored on a bounding box, which this legacy
	// single-source CLI has no way to express -- and upstream rejects a search with
	// no bbox. Rather than invent flags for it, point the operator at the config
	// file the deployment already uses. Fail loudly rather than "succeed" having
	// collected nothing; land still SURFACES from stored items either way.
	if *cat == "land" {
		return fmt.Errorf("land ingest is not available from these legacy flags: the land connector (zillapi) needs a bounding box, so configure it as a source in a config.json and run `nagus serve -config ...`; land can still be searched/served from stored items")
	}
	// wine ingest likewise: a wine source must declare its shipping channel
	// and home state for the legal-destination stamp, which these flags
	// cannot express -- and defaulting a legality field is exactly the wrong
	// move.
	if *cat == "wine" {
		return fmt.Errorf("wine ingest is not available from these legacy flags: a wine source must declare wineChannel (winery_direct|retailer) and state (USPS code) in a config.json and run `nagus serve -config ...`; wine can still be searched/served from stored items")
	}

	sc := SourceConfig{
		Name:     *cat,
		Category: *cat,
		Type:     "ebay",
		Query:    *query,
		Limit:    *limit,
		Fixture:  *fixture,
	}

	st, closeSt, err := sflags.open(context.Background())
	if err != nil {
		return err
	}
	defer closeSt()
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "  "+format+"\n", a...) }
	opts := categoryOptsFromEnv(false, http.DefaultClient, logf)
	// runIngest's -client-id/-client-secret flags are this subcommand's own
	// credential source (unlike serve, which only reads NAGUS_EBAY_CLIENT_ID/
	// SECRET env); preserve that by taking the flags outright, matching the old
	// buildSourceConnector(sourceParams{...}) behavior exactly.
	opts.ebayClientID = *clientID
	opts.ebaySecret = *clientSecret

	ing, err := buildIngester(sc, categoryConfigFromOpts(sc.Category, opts), st, opts)
	if err != nil {
		return err
	}

	res, err := ing.Ingest(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("ingest[%s]: fetched=%d stored=%d skipped=%d (backend=%s)\n",
		*cat, res.Fetched, res.Stored, len(res.Skips), *sflags.backend)
	for _, s := range res.Skips {
		fmt.Printf("  skip %s at %s: %s\n", s.SourceKey, s.Stage, s.Reason)
	}
	return nil
}

func buildEbayConnector(name, fixture, clientID, clientSecret, query, marketplace string, limit int) (listing.Connector, error) {
	if fixture != "" {
		return ebay.NewConnector(ebay.Config{Name: name, FixturePath: fixture}), nil
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("live ingest needs -client-id and -client-secret (or use -ebay-fixture for offline)")
	}
	sandbox := envBool("NAGUS_EBAY_SANDBOX")
	cfg := ebay.Config{
		Name:          name,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		MarketplaceID: marketplace,
		Query:         query,
		Limit:         limit,
		// 0 -> ebay.DefaultDailyBudget (~5k/day prod cap, License 2.4).
		DailyBudget: int(envInt64("NAGUS_EBAY_DAILY_BUDGET", 0)),
		// NAGUS_EBAY_SANDBOX routes to the eBay Sandbox (License 8.4 test env) with
		// sandbox Application Keys, so validation runs don't spend the prod budget.
		Sandbox: sandbox,
	}
	// NAGUS_EBAY_SELLER_PROFILE opts into the per-seller second-call enrichment
	// (account-age + recent-sales tiers). OFF by default: each distinct seller
	// costs one budgeted call. The username is passed transiently and never stored.
	if envBool("NAGUS_EBAY_SELLER_PROFILE") {
		cfg.SellerProfile = ebay.NewShoppingProfileSource(clientID, sandbox, nil)
	}
	return ebay.NewConnector(cfg), nil
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
	cat := fs.String("category", "hdd", "category to search (hdd|land|wine)")
	sflags := registerStoreFlags(fs)
	text := fs.String("text", "", "case-insensitive text match over title/tokens")
	minCap := fs.Float64("min-capacity", category.DefaultMinCapacityTB, "hdd hard-filter capacity floor in TB")
	limit := fs.Int("limit", 20, "max items to surface")
	offline := fs.Bool("offline", false, "hdd: score against the built-in demo reference instead of the live feed")
	asJSON := fs.Bool("json", false, "emit results as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !supportedCategory(*cat) {
		return fmt.Errorf("unsupported category %q (want hdd or land)", *cat)
	}

	st, closeSt, err := sflags.open(context.Background())
	if err != nil {
		return err
	}
	defer closeSt()
	opts := categoryOptsFromEnv(*offline, http.DefaultClient, nil)
	opts.hddMinCapacity = *minCap
	cc := categoryConfigFromOpts(*cat, opts)
	sf, err := buildSurface(*cat, cc, st, opts)
	if err != nil {
		return err
	}

	q := store.Query{Category: *cat, Text: *text, Limit: *limit}
	res, err := sf.Surface(context.Background(), q)
	if err != nil {
		return err
	}
	if *asJSON {
		return emitJSON(res)
	}
	emitTable(*cat, res)
	return nil
}

func emitTable(cat string, res pipeline.SurfaceResult) {
	fmt.Printf("search[%s]: matched=%d survived-filter=%d\n", cat, res.Matched, res.Filtered)
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
