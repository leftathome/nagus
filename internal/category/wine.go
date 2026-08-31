package category

import (
	"context"
	"fmt"
	"strings"
	"time"

	extwine "github.com/leftathome/nagus/internal/extract/wine"
	"github.com/leftathome/nagus/internal/identity/lwin"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/shipping"
	"github.com/leftathome/nagus/internal/store"
	valwine "github.com/leftathome/nagus/internal/valuation/wine"
)

// The wine bundle (docs/design/2026-08-30-wine-category.md) scores wine
// QUALITY-RELATIVE-TO-PRICE: critic attributions parsed at extract time are
// normalized onto a common 100-point scale, and a hedonic log-price model
// places each listing's price against its quality cohort -- negative residual
// = cheap for the quality. Shipping legality is a first-class, fail-closed
// CONSTRAINT LAYER (internal/shipping), not a hardcoded rule for any one
// market: each source declares its channel + origin JURISDICTION ("US-WA",
// "FR", "CA-BC"), the data-driven rules table computes which destinations it
// may legally ship to anywhere in the world, and a surface configured with a
// destination (ShipTo -- the operator's own jurisdiction, or a gift
// recipient's) hard-filters to offers legal for THAT destination. An offer
// that cannot legally ship to the destination must never surface as
// actionable.

// channelTagger wraps a connector and stamps the source's shipping
// declaration (channel + origin jurisdiction) and its computed legal-destination set
// onto every Raw's aspects, where the wine extractor lifts them into typed
// attributes. Stamping at the connector seam keeps the extractor
// source-agnostic and makes the legality provenance auditable per listing.
// The whole destination SET is stamped (not one destination's boolean) so a
// single ingested corpus serves watches for any destination; a rules-table
// change converges on the next poll's re-stamp.
type channelTagger struct {
	inner listing.Connector
	src   shipping.Source
	rules shipping.Rules
}

// TagWineChannel wraps conn so every listing it emits carries the source's
// declared channel, origin jurisdiction, and the destination jurisdictions it
// may legally ship to under rules.
func TagWineChannel(conn listing.Connector, src shipping.Source, rules shipping.Rules) listing.Connector {
	return &channelTagger{inner: conn, src: src, rules: rules}
}

func (t *channelTagger) SourceID() string { return t.inner.SourceID() }

func (t *channelTagger) Fetch(ctx context.Context) ([]listing.Raw, error) {
	raws, err := t.inner.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	legalTo := strings.Join(t.rules.LegalDestinations(t.src), " ")
	for i := range raws {
		if raws[i].Aspects == nil {
			raws[i].Aspects = map[string]string{}
		}
		raws[i].Aspects["wine_channel"] = string(t.src.Channel)
		raws[i].Aspects["source_origin"] = t.src.Origin.Code()
		raws[i].Aspects["ship_legal_to"] = legalTo
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
	// ShipTo, when set to a jurisdiction code ("US-WA", "FR", "CA-BC"),
	// hard-filters to offers whose source may legally ship wine to a consumer
	// THERE (per the shipping rules the items were stamped with). It is the
	// buyer's jurisdiction, not the operator's home: set it to a gift
	// recipient's to shop for them. Fail closed: items with no stamped
	// legal-destination set are dropped. Empty = no legality filter (every
	// offer is informational).
	ShipTo string
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
	// Rates converts a listing's currency into the model's (see
	// valwine.Valuer.Rates). Without a rate, a foreign-currency listing is
	// reported unplaceable rather than mispriced -- which matters as soon as
	// sources are international.
	Rates map[string]float64
	// Ship is the shipping-legality rules table used to stamp each source's
	// legal destinations at ingest. Nil = shipping.DefaultRules (the
	// documented baseline); inject an operator override (DefaultRules +
	// LoadRules + Override) when a destination's law has drifted.
	Ship  *shipping.Rules
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
	if cfg.ShipTo != "" {
		// An unparseable destination normalizes to a token nothing can carry,
		// so the filter surfaces nothing rather than everything. Config paths
		// reject it at startup (Rules.Modeled) so it never gets this far.
		dest, _ := shipping.NormJurisdiction(cfg.ShipTo)
		f.HasToken = map[string]string{"ship_legal_to": dest}
	}
	return f
}

// NewWineSurface builds the wine surface (read half): hard-filter + hedonic
// quality/value scoring.
func NewWineSurface(deps WineDeps) *pipeline.Surface {
	valuer := valwine.Valuer{Model: deps.Model, MinScores: deps.Score.MinScoreCount, Rates: deps.Rates}
	valuate := func(_ context.Context, it item.Item) (score.DealSignal, error) {
		wineScore, _ := parseFloatAttr(it, "wine_score")
		scoreCount := 0
		if n, ok := parseFloatAttr(it, "wine_score_count"); ok {
			scoreCount = int(n)
		}
		val, err := valuer.Value(wineScore, scoreCount, price750Equivalent(it), it.Currency)
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

// shipRules resolves the deps' rules table, defaulting to the documented
// baseline.
func (d WineDeps) shipRules() shipping.Rules {
	if d.Ship != nil {
		return *d.Ship
	}
	return shipping.DefaultRules()
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

// NewWineIngester builds the wine ingest half for one source connector. src
// is the source's shipping declaration (channel + origin jurisdiction); it
// must validate (legality must be a conscious per-source declaration, so an
// unknown channel or a malformed origin is a startup error, not a default).
func NewWineIngester(conn listing.Connector, src shipping.Source, deps WineDeps) (*pipeline.Ingester, error) {
	if err := src.Validate(); err != nil {
		return nil, fmt.Errorf("wine: %w", err)
	}
	return &pipeline.Ingester{
		Connector:        TagWineChannel(conn, src, deps.shipRules()),
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
