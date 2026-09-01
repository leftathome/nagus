// Package wine is the quality + value adapter for the "wine" category.
//
// It has two halves, both deterministic (no I/O, no LLM):
//
//  1. CRITIC-SCORE NORMALIZATION: raw critic attributions ("WS 92", "JR 17")
//     arrive on different scales (Wine Spectator 70-100, Jancis Robinson
//     12-20) and with different critic generosity. Normalize lifts each onto
//     a common 100-point scale (piecewise-linear anchors for the 20-point
//     scale, per-critic bias offsets); Aggregate averages the normalized
//     scores. Published inter-critic agreement is only r ~ 0.34-0.60
//     (Journal of Wine Economics: Ashton 2012/2013, Luxen 2018, Masset et
//     al. 2015), so aggregation genuinely reduces noise -- and a single
//     critic's score is never treated as ground truth: value flagging
//     requires MinScoresForQuality normalized scores by default (the same
//     minimum-3 rule Global Wine Score applies).
//
//  2. HEDONIC VALUE MODEL: wine price is strongly non-linear in score. The
//     JWE "superstar wines" result (266k Wine Spectator reviews) found the
//     price premium statistically significant only above 90 points, and Ali,
//     Lecocq & Visser (2008) found the critic effect "non-existent for
//     low-scored wines", rising steeply with grade. A naive points-per-dollar
//     ratio therefore fails; HedonicModel predicts log(price) with a
//     quadratic score term plus a 90-point superstar indicator, and the
//     verdict comes from the RESIDUAL (actual log price minus predicted) in
//     units of the model's residual spread -- negative residual = cheap for
//     its quality cohort.
//
// The default model coefficients are BOOTSTRAP PRIORS approximating the
// public Wine Enthusiast 130k review set (cold start, spec Stage 3): they are
// documented, tunable data -- not a fit performed in this package -- and are
// expected to be refit offline as live offer observations accrue.
//
// # Currency
//
// A hedonic model is fit in ONE currency (HedonicModel.Currency, default
// USD), so a listing priced in another currency cannot be compared against it
// directly. Once offers can come from outside that currency's market -- which
// they can, since sources are international -- silently treating 30 EUR as
// 30 USD would produce a confidently wrong verdict, the exact failure mode
// this codebase keeps re-learning. Valuer therefore converts using a
// configured rate, and where no rate is configured it returns
// VerdictUnknownNoReference: "we cannot place this" is a first-class answer,
// a mispriced deal flag is not.
package wine

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Verdict is the categorical deal-quality signal produced by Valuer.Value.
// The vocabulary intentionally matches internal/score's DealSignal taxonomy.
type Verdict string

const (
	VerdictGreat  Verdict = "great"
	VerdictGood   Verdict = "good"
	VerdictMarket Verdict = "market"
	VerdictPoor   Verdict = "poor"
	// VerdictUnknownNoReference means the value model could not place the
	// listing: no aggregated quality score, or fewer normalized scores than
	// the quality minimum. A first-class "we don't know", not an error.
	VerdictUnknownNoReference Verdict = "unknown-no-reference"
	// VerdictUnknownNoPrice means the listing price is unknown (0 per
	// item.Item's convention). Never take log of an unknown price.
	VerdictUnknownNoPrice Verdict = "unknown-no-price"
)

// MinScoresForQuality is the default minimum count of normalized critic
// scores required before quality-based value flagging is trusted (the GWS
// minimum-3 rule; see package doc). Operators can lower it via
// Valuer.MinScores at the cost of flagging on a single critic's opinion.
const MinScoresForQuality = 3

// RawScore is one critic attribution as parsed from a listing or fetched from
// a quality source: the critic's canonical code, the raw score, and the scale
// it was given on (100 or 20).
type RawScore struct {
	Critic string  // canonical critic code, e.g. "WS", "JS", "JR"
	Score  float64 // raw score on the critic's scale
	Scale  int     // 100 or 20
}

// anchor is one point of the 20-point -> 100-point piecewise-linear mapping.
type anchor struct{ from20, to100 float64 }

// anchors20 maps the 20-point scale (Jancis Robinson and most UK critics)
// onto the 100-point scale. Robinson publishes only a word scale and resists
// a fixed numeric table, so these are approximate, tunable anchors: the low
// end follows Cardebat-Paroissien's CDF mapping (their 12/20 lands at ~76
// /100), the top end pins 20/20 to 100. NOT a linear 5x -- the 20-point scale
// compresses the top in a way 5x badly overstates mid-range wines.
var anchors20 = []anchor{
	{12, 76},
	{15, 84},
	{16, 88},
	{17, 90},
	{18, 92.5},
	{18.5, 94.5},
	{19, 95.5},
	{20, 100},
}

// Normalizer converts RawScores onto the common 100-point scale. The zero
// value is usable: default anchors, no per-critic bias correction.
type Normalizer struct {
	// CriticBias, when set, is subtracted from a critic's 100-point-scale
	// score before aggregation (positive = a generous critic whose scores are
	// deflated). This is the cheap stand-in for full per-critic CDF
	// standardization; populate it from historical distributions when
	// available, leave empty to trust raw scores.
	CriticBias map[string]float64
}

// Normalize lifts one RawScore onto the 100-point scale. ok=false means the
// score is outside the plausible published range for its scale (100-point
// scores below 50 or above 100; 20-point outside [10, 20]) or the scale is
// unrecognized -- a wrong or garbled attribution, dropped rather than
// poisoning the aggregate.
func (n Normalizer) Normalize(r RawScore) (float64, bool) {
	var s100 float64
	switch r.Scale {
	case 100:
		if r.Score < 50 || r.Score > 100 {
			return 0, false
		}
		s100 = r.Score
	case 20:
		if r.Score < 10 || r.Score > 20 {
			return 0, false
		}
		s100 = interp20(r.Score)
	default:
		return 0, false
	}
	if bias, ok := n.CriticBias[strings.ToUpper(strings.TrimSpace(r.Critic))]; ok {
		s100 -= bias
	}
	return s100, true
}

// interp20 piecewise-linearly interpolates a 20-point score through
// anchors20, clamping below the first anchor.
func interp20(s float64) float64 {
	if s <= anchors20[0].from20 {
		return anchors20[0].to100
	}
	for i := 1; i < len(anchors20); i++ {
		lo, hi := anchors20[i-1], anchors20[i]
		if s <= hi.from20 {
			frac := (s - lo.from20) / (hi.from20 - lo.from20)
			return lo.to100 + frac*(hi.to100-lo.to100)
		}
	}
	return anchors20[len(anchors20)-1].to100
}

// Aggregate normalizes and averages raw scores, keeping AT MOST ONE score
// per critic (the highest -- retailers frequently repeat an attribution in
// title and body, and double-counting one critic would defeat the point of
// aggregating independent opinions). Unparseable/out-of-range scores are
// dropped. Returns the unweighted mean of the surviving normalized scores
// and how many critics contributed; n == 0 means no usable score.
func (n Normalizer) Aggregate(raw []RawScore) (mean float64, count int) {
	perCritic := map[string]float64{}
	for _, r := range raw {
		s100, ok := n.Normalize(r)
		if !ok {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(r.Critic))
		if key == "" {
			continue
		}
		if prev, seen := perCritic[key]; !seen || s100 > prev {
			perCritic[key] = s100
		}
	}
	if len(perCritic) == 0 {
		return 0, 0
	}
	keys := make([]string, 0, len(perCritic))
	for k := range perCritic {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sum float64
	for _, k := range keys {
		sum += perCritic[k]
	}
	return sum / float64(len(perCritic)), len(perCritic)
}

// HedonicModel predicts log(price in cents) for a 750ml-equivalent bottle
// from its aggregated quality score:
//
//	log(price_cents) = Intercept
//	                 + ScoreCoef   * (score - 80)
//	                 + ScoreSqCoef * (score - 80)^2
//	                 + Superstar90Coef * 1[score >= 90]
//
// The quadratic + indicator shape is the point (see package doc): the
// superstar premium only exists above ~90 and nearly all published scores
// compress into 85-95, so linear score terms mis-price both tails.
type HedonicModel struct {
	Intercept       float64 // log cents at score 80
	ScoreCoef       float64 // per point above 80
	ScoreSqCoef     float64 // per squared point above 80
	Superstar90Coef float64 // step premium at >= 90 points
	// Currency is the ISO 4217 code the coefficients were fit in; "" means
	// DefaultCurrency. A listing in any other currency needs a Valuer rate.
	Currency string
	// ResidualStd is the spread of log-price residuals the model was fit
	// with; residual z-scores (the verdict input) are residual/ResidualStd.
	ResidualStd float64
}

// DefaultModel returns the bootstrap-prior model (see package doc): rough
// anchor points $18 @ 85, $30 @ 90, $73 @ 95, $245 @ 100 on the Wine
// Enthusiast 130k distribution (mean score ~88.4, log-price residual spread
// ~0.45). Refit offline and inject as live data accrues.
func DefaultModel() HedonicModel {
	return HedonicModel{
		Intercept:       7.40, // ~ $16.4 at 80 points
		ScoreCoef:       -0.02,
		ScoreSqCoef:     0.008,
		Superstar90Coef: 0.15,
		ResidualStd:     0.45,
		Currency:        DefaultCurrency,
	}
}

// currency returns the model's currency, defaulting to DefaultCurrency.
func (m HedonicModel) currency() string {
	if c := normCurrency(m.Currency); c != "" {
		return c
	}
	return DefaultCurrency
}

// PredictLogPriceCents returns the model's predicted log(price in cents) for
// an aggregated 100-point score.
func (m HedonicModel) PredictLogPriceCents(score float64) float64 {
	s := score - 80
	p := m.Intercept + m.ScoreCoef*s + m.ScoreSqCoef*s*s
	if score >= 90 {
		p += m.Superstar90Coef
	}
	return p
}

// Valuation is the result of placing one listing against the hedonic model.
type Valuation struct {
	Verdict Verdict

	// ResidualZ is (log actual - log predicted) / ResidualStd. Negative =
	// cheaper than the model expects for the quality. Only meaningful when
	// HasReference.
	ResidualZ float64
	// Ratio is actual price / predicted price (same convention as the other
	// categories: lower = better deal). Only meaningful when HasReference.
	Ratio float64
	// PredictedPriceCents is the model's expected price. 0 when !HasReference.
	PredictedPriceCents int64
	// HasReference reports whether the model could place the listing at all.
	HasReference bool
}

// ErrInvalidModel is returned when the model's ResidualStd is not positive; a
// zero spread would make every residual z infinite and hide a config bug.
var ErrInvalidModel = errors.New("wine: hedonic model ResidualStd must be > 0")

// Valuer computes Valuations for wine listings. The zero value uses
// DefaultModel and the MinScoresForQuality minimum.
type Valuer struct {
	// Model, when nil, defaults to DefaultModel().
	Model *HedonicModel
	// MinScores is the minimum count of normalized critic scores required
	// before the model flags value; 0 defaults to MinScoresForQuality (3).
	// A listing below the minimum gets unknown-no-reference: never flag a
	// "deal" on one critic's opinion (package doc).
	MinScores int

	// Rates converts a listing currency into the MODEL's currency: the value
	// is how many units of the model currency one unit of the listing
	// currency buys (e.g. with a USD model, {"EUR": 1.08, "GBP": 1.27}).
	// A listing already in the model currency needs no entry. A listing in
	// any other currency with no rate is UNPLACEABLE, not assumed 1:1 --
	// see the package doc's currency section.
	//
	// Rates are operator config, deliberately not fetched: a stale rate
	// shifts a verdict slightly, whereas a live FX dependency on the read
	// path could fail or hang a surface. Wine prices do not move on FX
	// intraday, so periodic config is the right granularity.
	Rates map[string]float64

	// Residual-z upper bounds per verdict tier; zero values take defaults.
	// Negative z = cheap for the quality.
	GreatMaxZ  float64 // default -1.5 (the spec's flag threshold)
	GoodMaxZ   float64 // default -0.5
	MarketMaxZ float64 // default +0.5; above this is poor
}

const (
	defaultGreatMaxZ  = -1.5
	defaultGoodMaxZ   = -0.5
	defaultMarketMaxZ = 0.5
)

func (v Valuer) thresholds() (great, good, market float64) {
	great, good, market = v.GreatMaxZ, v.GoodMaxZ, v.MarketMaxZ
	if great == 0 {
		great = defaultGreatMaxZ
	}
	if good == 0 {
		good = defaultGoodMaxZ
	}
	if market == 0 {
		market = defaultMarketMaxZ
	}
	return great, good, market
}

func (v Valuer) minScores() int {
	if v.MinScores > 0 {
		return v.MinScores
	}
	return MinScoresForQuality
}

// Value places one listing: its aggregated normalized score, how many critic
// scores contributed, and its price in minor units of currency (0 == unknown
// price). Price should be normalized to a 750ml-equivalent bottle by the
// caller when the size is known and differs.
//
// currency is the listing's ISO 4217 code. An EMPTY currency is read as the
// model's own -- a connector that omits the field is a data gap, not evidence
// of a foreign currency, and darkening every such item would be worse than
// the assumption. A named currency that is neither the model's nor in Rates
// yields VerdictUnknownNoReference rather than a wrong verdict.
func (v Valuer) Value(score float64, scoreCount int, priceCents int64, currency string) (Valuation, error) {
	if priceCents < 0 {
		return Valuation{}, fmt.Errorf("wine: price_cents must be >= 0, got %d", priceCents)
	}
	if priceCents == 0 {
		return Valuation{Verdict: VerdictUnknownNoPrice}, nil
	}
	if scoreCount < v.minScores() || score <= 0 {
		return Valuation{Verdict: VerdictUnknownNoReference}, nil
	}

	model := DefaultModel()
	if v.Model != nil {
		model = *v.Model
	}
	if model.ResidualStd <= 0 {
		return Valuation{}, ErrInvalidModel
	}

	priceInModelCurrency, ok := v.convert(priceCents, currency, model.currency())
	if !ok {
		// A foreign-currency listing with no configured rate cannot be
		// placed against this model. Unplaceable, never mispriced.
		return Valuation{Verdict: VerdictUnknownNoReference}, nil
	}

	predictedLog := model.PredictLogPriceCents(score)
	residual := math.Log(priceInModelCurrency) - predictedLog
	z := residual / model.ResidualStd

	great, good, market := v.thresholds()
	var verdict Verdict
	switch {
	case z <= great:
		verdict = VerdictGreat
	case z <= good:
		verdict = VerdictGood
	case z <= market:
		verdict = VerdictMarket
	default:
		verdict = VerdictPoor
	}

	predicted := math.Exp(predictedLog)
	return Valuation{
		Verdict:             verdict,
		ResidualZ:           z,
		Ratio:               priceInModelCurrency / predicted,
		PredictedPriceCents: int64(math.Round(predicted)),
		HasReference:        true,
	}, nil
}

// DefaultCurrency is the currency DefaultModel's coefficients are fit in.
const DefaultCurrency = "USD"

// normCurrency canonicalizes an ISO 4217 code for comparison.
func normCurrency(c string) string {
	return strings.ToUpper(strings.TrimSpace(c))
}

// convert restates priceCents in the model's currency. ok=false means the
// listing's currency is neither the model's nor rated, so the listing cannot
// be placed. The returned value is a float because an FX conversion has no
// business pretending to minor-unit exactness; it feeds a log() immediately.
func (v Valuer) convert(priceCents int64, listingCurrency, modelCurrency string) (float64, bool) {
	lc := normCurrency(listingCurrency)
	if lc == "" || lc == modelCurrency {
		return float64(priceCents), true
	}
	rate, ok := v.Rates[lc]
	if !ok {
		// Try the canonicalized key too, so a config written as {"eur": ...}
		// still works.
		for k, r := range v.Rates {
			if normCurrency(k) == lc {
				rate, ok = r, true
				break
			}
		}
	}
	if !ok || rate <= 0 {
		return 0, false
	}
	return float64(priceCents) * rate, true
}
