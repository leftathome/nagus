package main

import (
	"fmt"
	"net/http"

	"github.com/leftathome/nagus/internal/category"
	"github.com/leftathome/nagus/internal/connector/craigslist"
	"github.com/leftathome/nagus/internal/enrich/parcel"
	"github.com/leftathome/nagus/internal/listing"
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

// buildConnectorForSource builds the collection connector for one source by type.
func buildConnectorForSource(s SourceConfig, o categoryOpts) (listing.Connector, error) {
	switch s.Type {
	case "ebay":
		// Default query is HDD-specific; eBay currently only feeds the hdd category.
		conn, err := buildEbayConnector(s.Name, s.Fixture, o.ebayClientID, o.ebaySecret, orDefault(s.Query, "internal hard drive"), "EBAY_US", orInt(s.Limit, 50))
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", s.Name, err)
		}
		return conn, nil
	case "craigslist":
		clCat := orDefault(s.ClCategory, "reo")
		if s.Fixture != "" {
			return craigslist.NewConnector(craigslist.Config{Name: s.Name, FixturePath: s.Fixture, Category: clCat}), nil
		}
		if s.City == "" {
			return nil, fmt.Errorf("source %q: craigslist needs city or fixture", s.Name)
		}
		return craigslist.NewConnector(craigslist.Config{Name: s.Name, City: s.City, Category: clCat}), nil
	default:
		return nil, fmt.Errorf("source %q: unsupported type %q", s.Name, s.Type)
	}
}

// buildIngester builds the ingest unit for one source (connector by type, bundle by category).
func buildIngester(s SourceConfig, st store.Store, o categoryOpts) (*pipeline.Ingester, error) {
	conn, err := buildConnectorForSource(s, o)
	if err != nil {
		return nil, err
	}
	switch s.Category {
	case "hdd":
		return category.NewHDDIngester(conn, category.HDDDeps{Store: st, HTTPClient: o.http, Logf: o.logf}), nil
	case "land":
		return category.NewLandIngester(conn, category.LandDeps{Store: st, Logf: o.logf}), nil
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
