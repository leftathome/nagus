package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/leftathome/nagus/internal/category"
	"github.com/leftathome/nagus/internal/connector/shopify"
	"github.com/leftathome/nagus/internal/connector/zillapi"
	"github.com/leftathome/nagus/internal/enrich/parcel"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
)

// supportedCategory reports whether a category bundle is wired into the CLI.
func supportedCategory(cat string) bool { return cat == "hdd" || cat == "land" }

// categoryOpts is the per-category runtime config. hdd fields come from the
// -offline flag; land scoring/enrichment config comes from env (NAGUS_LAND_*,
// NAGUS_RENTCAST_KEY); ebayClientID/ebaySecret come from env
// (NAGUS_EBAY_CLIENT_ID/NAGUS_EBAY_CLIENT_SECRET) -- this is how the Helm
// chart configures it.
type categoryOpts struct {
	logf func(string, ...any)
	http *http.Client

	hddOffline     bool
	hddMinCapacity float64

	landBudgetCents int64
	landMinAcreage  float64
	landMaxAcreage  float64
	rentcastKey     string
	zillapiKey      string
	// offers is the optional offer layer; nil disables it.
	offers offer.Store

	ebayClientID string
	ebaySecret   string
}

// landOptsFromEnv fills the land scoring/enrichment config from env.
func categoryOptsFromEnv(hddOffline bool, client *http.Client, logf func(string, ...any)) categoryOpts {
	return categoryOpts{
		logf:            logf,
		http:            client,
		hddOffline:      hddOffline,
		landBudgetCents: envInt64("NAGUS_LAND_BUDGET_CENTS", 0),
		landMinAcreage:  envFloat("NAGUS_LAND_MIN_ACREAGE", category.DefaultMinAcreageAcres),
		landMaxAcreage:  envFloat("NAGUS_LAND_MAX_ACREAGE", 0),
		rentcastKey:     envOr("NAGUS_RENTCAST_KEY", ""),
		zillapiKey:      envOr("NAGUS_ZILLAPI_KEY", ""),
		ebayClientID:    envOr("NAGUS_EBAY_CLIENT_ID", ""),
		ebaySecret:      envOr("NAGUS_EBAY_CLIENT_SECRET", ""),
	}
}

// orDefault returns def when s is empty, else s.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// orInt returns def when n is zero, else n.
func orInt(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}

// categoryConfigFromOpts builds a CategoryConfig from the env-derived
// categoryOpts. It is the single place that maps env config onto the category
// window, used both by the legacy single-source path (which has no config.json to
// read a CategoryConfig from) and by resolveRunConfig when it synthesizes one.
func categoryConfigFromOpts(cat string, o categoryOpts) CategoryConfig {
	cc := CategoryConfig{}
	switch cat {
	case "hdd":
		cc.MinCapacityTB = o.hddMinCapacity
	case "land":
		cc.MinAcreageAcres = o.landMinAcreage
		cc.MaxAcreageAcres = o.landMaxAcreage
		cc.BudgetCents = o.landBudgetCents
	}
	return cc
}

// buildConnectorForSource builds the collection connector for one source by type.
// cc is the CategoryConfig of the source's category: a connector that can push a
// filter upstream (zillapi) derives its window from the SAME config the local
// hard-filter uses, so the two cannot drift apart.
func buildConnectorForSource(s SourceConfig, cc CategoryConfig, o categoryOpts) (listing.Connector, error) {
	switch s.Type {
	case "ebay":
		// Default query is HDD-specific; eBay currently only feeds the hdd category.
		conn, err := buildEbayConnector(s.Name, s.Fixture, o.ebayClientID, o.ebaySecret, orDefault(s.Query, "internal hard drive"), "EBAY_US", orInt(s.Limit, 50))
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", s.Name, err)
		}
		return conn, nil
	case "zillapi":
		return buildZillapiConnector(s, cc, o)
	case "shopify":
		return buildShopifyConnector(s, o)
	default:
		return nil, fmt.Errorf("source %q: unsupported type %q", s.Name, s.Type)
	}
}

// buildZillapiConnector builds the land connector. The acreage floor and price
// ceiling come from cc (the category's own window) and are pushed UPSTREAM, which
// is a cost control and not just a filter: Zillapi bills one credit per result
// returned, so narrowing before the response is what keeps the bill down. See
// docs/design/2026-07-29-zillapi-land-connector.md.
func buildZillapiConnector(s SourceConfig, cc CategoryConfig, o categoryOpts) (listing.Connector, error) {
	cfg := zillapi.Config{
		Name:         s.Name,
		APIKey:       o.zillapiKey,
		MinLotAcres:  cc.MinAcreageAcres,
		MaxPriceUSD:  int(cc.BudgetCents / 100),
		DaysOnZillow: s.DaysOnZillow,
		MaxItems:     s.MaxItems,
		HTTPClient:   o.http,
		FixturePath:  s.Fixture,
	}
	if s.BBox != nil {
		cfg.BBox = zillapi.BBox{West: s.BBox.West, South: s.BBox.South, East: s.BBox.East, North: s.BBox.North}
	}
	if s.Fixture == "" {
		// Live mode preconditions, checked here so a misconfigured source fails at
		// startup with a clear message instead of burning a call on a 400.
		if s.BBox == nil {
			return nil, fmt.Errorf("source %q: zillapi needs a bbox (upstream rejects a search with no bounding box) or a fixture", s.Name)
		}
		if o.zillapiKey == "" {
			return nil, fmt.Errorf("source %q: zillapi needs NAGUS_ZILLAPI_KEY set", s.Name)
		}
	}
	return zillapi.NewConnector(cfg), nil
}

// buildShopifyConnector builds one store's connector. There are no credentials:
// products.json is a public, unauthenticated storefront feed. Storefronts DO rate
// limit (serverpartdeals returns 429 "local_rate_limited" readily), so pair this
// with a polite interval -- hourly at most per store.
func buildShopifyConnector(s SourceConfig, o categoryOpts) (listing.Connector, error) {
	if s.Fixture == "" && s.BaseURL == "" {
		return nil, fmt.Errorf("source %q: shopify needs baseUrl (the storefront root) or a fixture", s.Name)
	}
	return shopify.NewConnector(shopify.Config{
		Name:                s.Name,
		BaseURL:             s.BaseURL,
		ProductTypePrefixes: s.ProductTypePrefixes,
		IncludeUnavailable:  s.IncludeUnavailable,
		BrandTag:            s.BrandTag,
		SKUIsMPN:            s.SKUIsMPN,
		SKUSuffixes:         s.SKUSuffixes,
		MaxPages:            s.MaxPages,
		FixturePath:         s.Fixture,
		Logf:                o.logf,
	}), nil
}

// retentionForSource is the per-SOURCE retention policy table. Retention is a
// property of the source's terms, not of the category evaluating it (spec locked
// decision #3), so it is decided here from the source's TYPE rather than inside a
// category bundle.
//
//   - ebay: License 8.1(b) forbids retaining eBay Content past its short public
//     life, so items are purged at 6h and offers use `purge` on the same window.
//     The spec's intended end state is `summarize-decay`, but that requires a
//     compliance judgement about the summary schema and stays OFF until validated.
//   - everything else: no obligation to forget, so items are not purged and
//     offers are retained in full. Purging them would also have been actively
//     harmful -- a storefront that rate-limits us for a few hours would lose its
//     whole corpus for no reason.
//
// Offers are expired (not deleted) after a grace period of several poll
// intervals, so one failed poll never marks a live catalogue dead.
func retentionForSource(s SourceConfig) (staleAfter time.Duration, ret offer.Retention, expireAfter time.Duration) {
	interval := time.Duration(s.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = time.Hour
	}
	// Three missed polls before an offer is considered gone.
	expireAfter = 3 * interval
	switch s.Type {
	case "ebay":
		return category.EbayContentMaxAge,
			offer.Retention{Policy: offer.Purge, Window: category.EbayContentMaxAge},
			expireAfter
	default:
		return 0, offer.Retention{Policy: offer.RetainFull}, expireAfter
	}
}

// buildIngester builds the ingest unit for one source (connector by type, bundle by category).
func buildIngester(s SourceConfig, cc CategoryConfig, st store.Store, o categoryOpts) (*pipeline.Ingester, error) {
	conn, err := buildConnectorForSource(s, cc, o)
	if err != nil {
		return nil, err
	}
	staleAfter, offerRetention, expireAfter := retentionForSource(s)

	// OFFER-ONLY source: no category, therefore no evaluation machinery. It
	// records offers and stops (gate-at-eval, nagus-7yq). Refuse to build one
	// when the offer layer is off, because it would fetch on an interval and
	// throw everything away -- a silent no-op is worse than a startup error.
	if s.Category == "" {
		if o.offers == nil {
			return nil, fmt.Errorf("source %q: offer-only sources (no category) need the offer layer enabled", s.Name)
		}
		return &pipeline.Ingester{
			Connector:        conn,
			Offers:           o.offers,
			OfferRetention:   offerRetention,
			OfferExpireAfter: expireAfter,
			Logf:             o.logf,
		}, nil
	}

	switch s.Category {
	case "hdd":
		return category.NewHDDIngester(conn, category.HDDDeps{
			Store: st, HTTPClient: o.http, Logf: o.logf,
			StaleAfter: staleAfter, Offers: o.offers,
			OfferRetention: offerRetention, OfferExpireAfter: expireAfter,
		}), nil
	case "land":
		return category.NewLandIngester(conn, category.LandDeps{
			Store: st, Logf: o.logf,
			StaleAfter: staleAfter, Offers: o.offers,
			OfferRetention: offerRetention, OfferExpireAfter: expireAfter,
		}), nil
	default:
		return nil, fmt.Errorf("source %q: unsupported category %q", s.Name, s.Category)
	}
}

// buildSurface builds the surface unit for one category from its config.
func buildSurface(cat string, cc CategoryConfig, st store.Store, o categoryOpts) (*pipeline.Surface, error) {
	switch cat {
	case "hdd":
		deps := category.HDDDeps{Store: st, HTTPClient: o.http, MinCapacityTB: cc.MinCapacityTB, Logf: o.logf}
		if o.hddOffline {
			deps.Reference = demoReference
		}
		return category.NewHDDSurface(deps), nil
	case "land":
		deps := category.LandDeps{
			Store: st, HTTPClient: o.http, Logf: o.logf,
			Score: category.LandScoreConfig{BudgetCents: cc.BudgetCents, MinAcreageAcres: cc.MinAcreageAcres, MaxAcreageAcres: cc.MaxAcreageAcres},
		}
		if o.rentcastKey != "" {
			deps.Parcel = parcel.NewRentcastProvider(o.http, o.rentcastKey)
		}
		return category.NewLandSurface(deps), nil
	default:
		return nil, fmt.Errorf("unsupported category %q", cat)
	}
}
