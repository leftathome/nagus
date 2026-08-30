package wine

import (
	"math"
	"testing"
)

// --- Normalize ---

func TestNormalize_100PointPassthrough(t *testing.T) {
	n := Normalizer{}
	got, ok := n.Normalize(RawScore{Critic: "WS", Score: 92, Scale: 100})
	if !ok || got != 92 {
		t.Fatalf("expected 92 ok, got %v %v", got, ok)
	}
}

func TestNormalize_100PointRejectsImplausible(t *testing.T) {
	n := Normalizer{}
	for _, s := range []float64{49, 101, -5} {
		if _, ok := n.Normalize(RawScore{Critic: "WS", Score: s, Scale: 100}); ok {
			t.Errorf("score %v on 100 scale should be rejected", s)
		}
	}
	// 50 is the documented floor (Decanter's scale starts at 50).
	if _, ok := n.Normalize(RawScore{Critic: "D", Score: 50, Scale: 100}); !ok {
		t.Errorf("score 50 should be accepted")
	}
}

func TestNormalize_20PointAnchors(t *testing.T) {
	n := Normalizer{}
	cases := []struct {
		in   float64
		want float64
	}{
		{12, 76},  // Cardebat-Paroissien low anchor
		{17, 90},  // mid anchor
		{20, 100}, // top pin
		{10, 76},  // clamped below the first anchor
		{18.5, 94.5},
	}
	for _, c := range cases {
		got, ok := n.Normalize(RawScore{Critic: "JR", Score: c.in, Scale: 20})
		if !ok {
			t.Fatalf("20-point %v should normalize", c.in)
		}
		if math.Abs(got-c.want) > 0.01 {
			t.Errorf("20-point %v: expected %v, got %v", c.in, c.want, got)
		}
	}
}

func TestNormalize_20PointInterpolatesBetweenAnchors(t *testing.T) {
	n := Normalizer{}
	// 16.5 is halfway between anchors 16->88 and 17->90.
	got, ok := n.Normalize(RawScore{Critic: "JR", Score: 16.5, Scale: 20})
	if !ok || math.Abs(got-89) > 0.01 {
		t.Fatalf("expected 89, got %v (ok=%v)", got, ok)
	}
}

func TestNormalize_20PointIsNotLinear5x(t *testing.T) {
	// The naive 5x conversion is the documented failure mode: 15/20 would be
	// 75/100 but the anchor table puts it at 84.
	n := Normalizer{}
	got, _ := n.Normalize(RawScore{Critic: "JR", Score: 15, Scale: 20})
	if got == 75 {
		t.Fatalf("20-point conversion must not be linear 5x")
	}
	if got != 84 {
		t.Fatalf("expected anchor value 84 for 15/20, got %v", got)
	}
}

func TestNormalize_UnknownScaleRejected(t *testing.T) {
	n := Normalizer{}
	if _, ok := n.Normalize(RawScore{Critic: "X", Score: 4, Scale: 5}); ok {
		t.Fatalf("unknown scale should be rejected")
	}
}

func TestNormalize_CriticBiasApplied(t *testing.T) {
	n := Normalizer{CriticBias: map[string]float64{"JS": 2.0}}
	got, ok := n.Normalize(RawScore{Critic: "js", Score: 94, Scale: 100})
	if !ok || got != 92 {
		t.Fatalf("expected bias-corrected 92 (case-insensitive critic), got %v ok=%v", got, ok)
	}
}

// --- Aggregate ---

func TestAggregate_MeanOfNormalizedScores(t *testing.T) {
	n := Normalizer{}
	mean, count := n.Aggregate([]RawScore{
		{Critic: "WS", Score: 92, Scale: 100},
		{Critic: "JS", Score: 94, Scale: 100},
		{Critic: "JR", Score: 17, Scale: 20}, // -> 90
	})
	if count != 3 {
		t.Fatalf("expected 3 contributing critics, got %d", count)
	}
	if math.Abs(mean-92) > 0.01 {
		t.Fatalf("expected mean 92, got %v", mean)
	}
}

func TestAggregate_DedupesPerCritic(t *testing.T) {
	// A retailer repeating "WS 92" in title and body must count once, and the
	// higher of two same-critic scores wins.
	n := Normalizer{}
	mean, count := n.Aggregate([]RawScore{
		{Critic: "WS", Score: 90, Scale: 100},
		{Critic: "ws", Score: 92, Scale: 100},
	})
	if count != 1 {
		t.Fatalf("same critic should count once, got %d", count)
	}
	if mean != 92 {
		t.Fatalf("expected the higher duplicate (92), got %v", mean)
	}
}

func TestAggregate_DropsGarbageAndEmptyCritic(t *testing.T) {
	n := Normalizer{}
	mean, count := n.Aggregate([]RawScore{
		{Critic: "WS", Score: 92, Scale: 100},
		{Critic: "JS", Score: 12, Scale: 100}, // implausible on 100 scale
		{Critic: "", Score: 95, Scale: 100},   // no critic identity
	})
	if count != 1 || mean != 92 {
		t.Fatalf("expected only WS 92 to survive, got mean=%v count=%d", mean, count)
	}
}

func TestAggregate_EmptyInput(t *testing.T) {
	n := Normalizer{}
	mean, count := n.Aggregate(nil)
	if mean != 0 || count != 0 {
		t.Fatalf("empty input should be 0,0; got %v,%d", mean, count)
	}
}

// --- HedonicModel ---

func TestHedonicModel_SuperstarPremiumIsNonLinear(t *testing.T) {
	m := DefaultModel()
	// The marginal value of a point must be much larger at the top than in
	// the middle of the scale (superstar premium): 94->95 must be worth more
	// in log terms than 84->85.
	low := m.PredictLogPriceCents(85) - m.PredictLogPriceCents(84)
	high := m.PredictLogPriceCents(95) - m.PredictLogPriceCents(94)
	if high <= low {
		t.Fatalf("expected steeper price curve at high scores: 84->85 %v, 94->95 %v", low, high)
	}
	// And the 90-point step premium must be visible: crossing 89->90 jumps by
	// more than the quadratic alone.
	step := m.PredictLogPriceCents(90) - m.PredictLogPriceCents(89)
	within := m.PredictLogPriceCents(89) - m.PredictLogPriceCents(88)
	if step <= within {
		t.Fatalf("expected a superstar step at 90: 89->90 %v, 88->89 %v", step, within)
	}
}

func TestHedonicModel_MonotoneAbove85(t *testing.T) {
	m := DefaultModel()
	prev := m.PredictLogPriceCents(85)
	for s := 86.0; s <= 100; s++ {
		cur := m.PredictLogPriceCents(s)
		if cur <= prev {
			t.Fatalf("predicted price should increase with score at %v", s)
		}
		prev = cur
	}
}

// --- Valuer.Value ---

func TestValue_UnknownNoPrice(t *testing.T) {
	v := Valuer{}
	val, err := v.Value(95, 5, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val.Verdict != VerdictUnknownNoPrice || val.HasReference {
		t.Fatalf("expected unknown-no-price without reference, got %+v", val)
	}
}

func TestValue_NegativePriceIsError(t *testing.T) {
	v := Valuer{}
	if _, err := v.Value(95, 5, -100); err == nil {
		t.Fatalf("negative price should error")
	}
}

func TestValue_MinScoresRuleGatesFlagging(t *testing.T) {
	v := Valuer{} // default MinScores = 3
	val, err := v.Value(95, 2, 2000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val.Verdict != VerdictUnknownNoReference {
		t.Fatalf("2 scores must not be enough to flag value, got %v", val.Verdict)
	}

	// An operator can lower the bar explicitly.
	v = Valuer{MinScores: 1}
	val, err = v.Value(95, 1, 2000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !val.HasReference {
		t.Fatalf("MinScores=1 should allow flagging on one score")
	}
}

func TestValue_ZeroScoreIsUnknown(t *testing.T) {
	v := Valuer{MinScores: 1}
	val, err := v.Value(0, 3, 2000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val.Verdict != VerdictUnknownNoReference {
		t.Fatalf("a zero aggregated score cannot be placed, got %v", val.Verdict)
	}
}

func TestValue_VerdictTiers(t *testing.T) {
	v := Valuer{}
	m := DefaultModel()
	// A 95-point wine's predicted price:
	predicted := math.Exp(m.PredictLogPriceCents(95))

	cases := []struct {
		name       string
		priceCents int64
		want       Verdict
	}{
		// z = ln(actual/predicted)/0.45; -1.5z is a factor ~0.51.
		{"half price for the quality", int64(predicted * 0.45), VerdictGreat},
		{"25 percent under", int64(predicted * 0.75), VerdictGood},
		{"at model price", int64(predicted), VerdictMarket},
		{"double the model price", int64(predicted * 2.0), VerdictPoor},
	}
	for _, c := range cases {
		val, err := v.Value(95, 3, c.priceCents)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", c.name, err)
		}
		if val.Verdict != c.want {
			t.Errorf("%s (z=%.2f): expected %v, got %v", c.name, val.ResidualZ, c.want, val.Verdict)
		}
		if !val.HasReference {
			t.Errorf("%s: expected HasReference", c.name)
		}
	}
}

func TestValue_RatioAndPredictedPrice(t *testing.T) {
	v := Valuer{}
	m := DefaultModel()
	predicted := math.Exp(m.PredictLogPriceCents(92))
	val, err := v.Value(92, 4, int64(predicted))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(val.Ratio-1.0) > 0.01 {
		t.Errorf("price == predicted should be ratio ~1, got %v", val.Ratio)
	}
	if math.Abs(float64(val.PredictedPriceCents)-predicted) > 1 {
		t.Errorf("predicted price cents %d does not match model %v", val.PredictedPriceCents, predicted)
	}
	if math.Abs(val.ResidualZ) > 0.01 {
		t.Errorf("price == predicted should be z ~0, got %v", val.ResidualZ)
	}
}

func TestValue_InvalidModelRejected(t *testing.T) {
	v := Valuer{Model: &HedonicModel{ResidualStd: 0}}
	if _, err := v.Value(95, 3, 2000); err == nil {
		t.Fatalf("a model with zero ResidualStd should error, not divide by zero")
	}
}
