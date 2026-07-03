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
// NAGUS_RENTCAST_KEY), which is how the Helm chart configures it.
type categoryOpts struct {
	logf func(string, ...any)
	http *http.Client

	hddOffline     bool
	hddMinCapacity float64

	landBudgetCents int64
	landMinAcreage  float64
	landMaxAcreage  float64
	rentcastKey     string
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
	}
}

// buildPipeline constructs the pipeline for a category. conn may be nil for a
// surface-only pipeline (search/watches).
func buildPipeline(cat string, conn listing.Connector, st store.Store, o categoryOpts) (*pipeline.Pipeline, error) {
	switch cat {
	case "hdd":
		deps := category.HDDDeps{Store: st, HTTPClient: o.http, MinCapacityTB: o.hddMinCapacity, Logf: o.logf}
		if o.hddOffline {
			deps.Reference = demoReference
		}
		return category.NewHDDPipeline(conn, deps), nil
	case "land":
		deps := category.LandDeps{
			Store: st, HTTPClient: o.http, Logf: o.logf,
			Score: category.LandScoreConfig{
				BudgetCents:     o.landBudgetCents,
				MinAcreageAcres: o.landMinAcreage,
				MaxAcreageAcres: o.landMaxAcreage,
			},
		}
		if o.rentcastKey != "" {
			deps.Parcel = parcel.NewRentcastProvider(o.http, o.rentcastKey)
		}
		return category.NewLandPipeline(conn, deps), nil
	default:
		return nil, fmt.Errorf("unsupported category %q (want hdd or land)", cat)
	}
}

// sourceParams carries the connector config for ingest (from flags) or serve
// (from env). Only the fields for the chosen category are consulted.
type sourceParams struct {
	// hdd (eBay)
	ebayFixture, ebayClientID, ebaySecret, ebayQuery string
	ebayLimit                                        int
	// land (Craigslist)
	clFixture, clCity, clCategory string
}

// buildSourceConnector returns the collection connector for a category.
func buildSourceConnector(cat string, p sourceParams) (listing.Connector, error) {
	switch cat {
	case "hdd":
		return buildEbayConnector(p.ebayFixture, p.ebayClientID, p.ebaySecret, p.ebayQuery, "EBAY_US", p.ebayLimit)
	case "land":
		cat := p.clCategory
		if cat == "" {
			cat = "reo"
		}
		if p.clFixture != "" {
			return craigslist.NewConnector(craigslist.Config{FixturePath: p.clFixture, Category: cat}), nil
		}
		if p.clCity == "" {
			return nil, fmt.Errorf("land ingest needs -craigslist-city or -craigslist-fixture")
		}
		return craigslist.NewConnector(craigslist.Config{City: p.clCity, Category: cat}), nil
	default:
		return nil, fmt.Errorf("unsupported category %q (want hdd or land)", cat)
	}
}
