package category

import (
	"context"
	"fmt"
	"strconv"
	"time"

	extwine "github.com/leftathome/nagus/internal/extract/wine"
	"github.com/leftathome/nagus/internal/identity/lwin"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
	valwine "github.com/leftathome/nagus/internal/valuation/wine"
)

// The wine bundle (docs/design/2026-08-30-wine-category.md) scores wine
// QUALITY-RELATIVE-TO-PRICE: critic attributions parsed at extract time are
// normalized onto a common 100-point scale, and a hedonic log-price model
// places each listing's price against its quality cohort -- negative residual
// = cheap for the quality. Washington shipping legality is a first-class,
// fail-closed gate (see WineChannel): an offer that cannot legally ship to a
// WA consumer must never be surfaced as actionable.

// WineChannel classifies a wine source's shipping channel for Washington
// legality. This is a property of the SOURCE (who ships, from where, under
// what license), not derivable from listing text -- so it is declared in the
// source config and stamped onto every listing by TagWineChannel.
//
// The legal background (RCW 66.20): out-of-state WINERIES holding a WA wine
// shipper permit may ship to WA consumers; IN-STATE licensed retailers may
// ship/deliver within WA; out-of-state RETAILERS may NOT (SB 5007, which
// would have created such a permit, died in committee Jan 2024). nagus only
// surfaces -- it never buys -- but surfacing an un-shippable offer as
// actionable would be a standing footgun, so legality is carried on every
// item and the filter can hard-require it.
type WineChannel string

const (
	// WineChannelWARetailer: an in-state (WA) licensed retailer. Legal.
	WineChannelWARetailer WineChannel = "wa_retailer"
	// WineChannelWineryDirect: a winery shipping under a WA wine shipper
	// permit (in- or out-of-state). Legal -- verify the permit per source
	// before enabling it (the permit is the source's, not nagus's, problem;
	// the config declaration records the operator's verification).
	WineChannelWineryDirect WineChannel = "winery_direct"
	// WineChannelOutOfStateRetailer: an out-of-state retailer. NOT legal to
	// ship to WA consumers; offers remain informational-only.
	WineChannelOutOfStateRetailer WineChannel = "out_of_state_retailer"
)

// ShipLegalWA reports whether this channel may legally ship wine to a WA
// consumer. Unknown channels are illegal by construction (fail closed).
func (c WineChannel) ShipLegalWA() bool {
	switch c {
	case WineChannelWARetailer, WineChannelWineryDirect:
		return true
	}
	return false
}

// Valid reports whether c is a declared channel value.
func (c WineChannel) Valid() bool {
	switch c {
	case WineChannelWARetailer, WineChannelWineryDirect, WineChannelOutOfStateRetailer:
		return true
	}
	return false
}

// channelTagger wraps a connector and stamps the source's wine channel and
// its derived WA-legality onto every Raw's aspects, where the wine extractor
// lifts them into typed attributes. Stamping at the connector seam keeps the
// extractor source-agnostic and makes the legality provenance auditable per
// listing.
type channelTagger struct {
	inner   listing.Connector
	channel WineChannel
}

// TagWineChannel wraps conn so every listing it emits carries the source's
// declared channel and WA ship-legality.
func TagWineChannel(conn listing.Connector, channel WineChannel) listing.Connector {
	return &channelTagger{inner: conn, channel: channel}
}

func (t *channelTagger) SourceID() string { return t.inner.SourceID() }

func (t *channelTagger) Fetch(ctx context.Context) ([]listing.Raw, error) {
	raws, err := t.inner.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	for i := range raws {
		if raws[i].Aspects == nil {
			raws[i].Aspects = map[string]string{}
		}
		raws[i].Aspects["wine_channel"] = string(t.channel)
		raws[i].Aspects["ship_legal_wa"] = strconv.FormatBool(t.channel.ShipLegalWA())
	}
	return raws, nil
}

// WineScoreConfig tunes the wine hard-filter and valuation. Zero values mean
// "no constraint" (BudgetCents, MinScore) or take the package default
// (MinScoreCount -> the valuation's minimum-3 rule).
type WineScoreConfig struct {
	// BudgetCents is the price ceiling; 0 = no budget.
	BudgetCents int64
	// MinScore, when > 0, hard-filters on the aggregated normalized critic
	// score (attribute wine_score). Items with NO score are then dropped at
	// the filter -- a watch that demands 92+ points cannot pass unscored
	// wine.
	MinScore float64
	// MinScoreCount is the minimum number of independent critic scores the
	// VALUATION requires before flagging value; 0 = the default minimum-3
	// rule (never flag a deal on one critic's opinion). Lowering it is an
	// explicit operator decision.
	MinScoreCount int
	// RequireShipLegalWA, when true, hard-filters to offers whose source
	// channel may legally ship to a WA consumer (fail closed: unstamped
	// items are dropped).
	RequireShipLegalWA bool
}

// WineDeps are the injectable dependencies of the wine bundle.
type WineDeps struct {
	Store store.Store
	// LWIN, when non-nil, resolves listings to LWIN canonical identities at
	// extract time. Nil = wine items carry no canonical id (identity join
	// disabled) -- the pipeline still works, per the graceful-degradation
	// convention.
	LWIN *lwin.Resolver
	// Model overrides the hedonic value model; nil = valwine.DefaultModel
	// (the documented cold-start bootstrap priors).
	Model *valwine.HedonicModel
	Score WineScoreConfig
	Logf  func(format string, args ...any)
	// --- per-source retention (same contract as the other bundles) ---
	StaleAfter       time.Duration
	Offers           offer.Store
	OfferRetention   offer.Retention
	OfferExpireAfter time.Duration
}

// WineFilter builds the deterministic hard-filter for the wine category.
// Priced is always required: every supported wine channel is retail, where an
// unpriced listing is dead weight, and the valuation could say nothing about
// it anyway.
func WineFilter(cfg WineScoreConfig) score.Filter {
	f := score.Filter{
		Category:      "wine",
		RequirePriced: true,
		MaxPriceCents: cfg.BudgetCents,
	}
	if cfg.MinScore > 0 {
		f.MinAttr = map[string]float64{"wine_score": cfg.MinScore}
	}
	if cfg.RequireShipLegalWA {
		f.EqAttr = map[string]string{"ship_legal_wa": "true"}
	}
	return f
}

// NewWineSurface builds the wine surface (read half): hard-filter + hedonic
// quality/value scoring.
func NewWineSurface(deps WineDeps) *pipeline.Surface {
	valuer := valwine.Valuer{Model: deps.Model, MinScores: deps.Score.MinScoreCount}
	valuate := func(_ context.Context, it item.Item) (score.DealSignal, error) {
		wineScore, _ := parseFloatAttr(it, "wine_score")
		scoreCount := 0
		if n, ok := parseFloatAttr(it, "wine_score_count"); ok {
			scoreCount = int(n)
		}
		val, err := valuer.Value(wineScore, scoreCount, price750Equivalent(it))
		if err != nil {
			return score.DealSignal{}, err
		}
		return score.DealSignal{
			Verdict:      string(val.Verdict),
			Ratio:        val.Ratio,
			HasReference: val.HasReference,
		}, nil
	}
	return &pipeline.Surface{
		Store:   deps.Store,
		Filter:  WineFilter(deps.Score),
		Valuate: valuate,
		Logf:    deps.Logf,
	}
}

// price750Equivalent scales the listing price to a standard-bottle
// equivalent so a magnum is not flagged "expensive for the quality" (the
// hedonic model predicts 750ml prices). A missing/invalid bottle_ml is
// treated as the standard bottle.
func price750Equivalent(it item.Item) int64 {
	ml, ok := parseFloatAttr(it, "bottle_ml")
	if !ok || ml <= 0 || ml == extwine.DefaultBottleML {
		return it.PriceCents
	}
	return int64(float64(it.PriceCents) * extwine.DefaultBottleML / ml)
}

// NewWineIngester builds the wine ingest half for one source connector.
// channel is the source's declared shipping channel; it must be a Valid
// WineChannel (legality must be a conscious per-source declaration, so an
// unknown value is a startup error, not a default).
func NewWineIngester(conn listing.Connector, channel WineChannel, deps WineDeps) (*pipeline.Ingester, error) {
	if !channel.Valid() {
		return nil, fmt.Errorf("wine: unknown channel %q (want %s|%s|%s)",
			channel, WineChannelWARetailer, WineChannelWineryDirect, WineChannelOutOfStateRetailer)
	}
	return &pipeline.Ingester{
		Connector:        TagWineChannel(conn, channel),
		Sanitizer:        sanitize.Passthrough{Name: "sanitize.passthrough(wine)"},
		Extractor:        &extwine.Extractor{Resolver: deps.LWIN},
		Store:            deps.Store,
		StaleAfter:       deps.StaleAfter,
		Offers:           deps.Offers,
		OfferRetention:   deps.OfferRetention,
		OfferExpireAfter: deps.OfferExpireAfter,
		Logf:             deps.Logf,
	}, nil
}
