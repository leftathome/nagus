package score

import (
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/item"
)

func baseItem() item.Item {
	return item.Item{
		ID:         "hdd-001",
		Category:   "hdd",
		Class:      item.ClassDurable,
		PriceCents: 20000,
		Condition:  "refurb",
		Attributes: map[string]string{
			"capacity_tb": "16",
		},
	}
}

// --- Filter.Pass ---

func TestFilterPass_EmptyFilterPassesEverything(t *testing.T) {
	cases := []item.Item{
		baseItem(),
		{ID: "unpriced", Category: "anything", PriceCents: 0},
		{ID: "no-attrs", Category: "x"},
	}
	f := Filter{}
	for _, it := range cases {
		ok, reason := f.Pass(it)
		if !ok {
			t.Errorf("item %q: expected empty filter to pass, got reason %q", it.ID, reason)
		}
		if reason != "" {
			t.Errorf("item %q: expected no reason on pass, got %q", it.ID, reason)
		}
	}
}

func TestFilterPass_Category(t *testing.T) {
	f := Filter{Category: "hdd"}

	it := baseItem()
	if ok, reason := f.Pass(it); !ok {
		t.Fatalf("matching category should pass, got reason %q", reason)
	}

	mismatch := baseItem()
	mismatch.Category = "land"
	ok, reason := f.Pass(mismatch)
	if ok {
		t.Fatalf("mismatched category should fail")
	}
	if !strings.Contains(reason, "category") {
		t.Errorf("reason should mention category, got %q", reason)
	}
}

func TestFilterPass_RequirePriced(t *testing.T) {
	f := Filter{RequirePriced: true}

	priced := baseItem()
	if ok, reason := f.Pass(priced); !ok {
		t.Fatalf("priced item should pass, got reason %q", reason)
	}

	unpriced := baseItem()
	unpriced.PriceCents = 0
	ok, reason := f.Pass(unpriced)
	if ok {
		t.Fatalf("unpriced item should fail when RequirePriced")
	}
	if !strings.Contains(reason, "price") {
		t.Errorf("reason should mention price, got %q", reason)
	}
}

func TestFilterPass_RequirePriced_NegativePriceStillFails(t *testing.T) {
	// item.Item.Validate() would already reject negative prices upstream, but
	// Pass should not panic or accidentally pass on a <= 0 price of any kind.
	f := Filter{RequirePriced: true}
	it := baseItem()
	it.PriceCents = -5
	ok, _ := f.Pass(it)
	if ok {
		t.Fatalf("negative price should fail RequirePriced")
	}
}

func TestFilterPass_MaxPriceCents(t *testing.T) {
	f := Filter{MaxPriceCents: 10000}

	underMax := baseItem()
	underMax.PriceCents = 9999
	if ok, reason := f.Pass(underMax); !ok {
		t.Fatalf("under max price should pass, got reason %q", reason)
	}

	atMax := baseItem()
	atMax.PriceCents = 10000
	if ok, reason := f.Pass(atMax); !ok {
		t.Fatalf("at max price should pass (inclusive bound), got reason %q", reason)
	}

	overMax := baseItem()
	overMax.PriceCents = 10001
	ok, reason := f.Pass(overMax)
	if ok {
		t.Fatalf("over max price should fail")
	}
	if !strings.Contains(reason, "price") {
		t.Errorf("reason should mention price, got %q", reason)
	}

	// Unpriced items are not judged against MaxPriceCents on their own.
	unpriced := baseItem()
	unpriced.PriceCents = 0
	if ok, reason := f.Pass(unpriced); !ok {
		t.Fatalf("unpriced item should not fail MaxPriceCents alone, got reason %q", reason)
	}
}

func TestFilterPass_MinAttr(t *testing.T) {
	f := Filter{MinAttr: map[string]float64{"capacity_tb": 8}}

	passing := baseItem() // capacity_tb = 16
	if ok, reason := f.Pass(passing); !ok {
		t.Fatalf("16 >= 8 should pass, got reason %q", reason)
	}

	tooSmall := baseItem()
	tooSmall.Attributes["capacity_tb"] = "4"
	ok, reason := f.Pass(tooSmall)
	if ok {
		t.Fatalf("4 < 8 should fail")
	}
	if !strings.Contains(reason, "capacity_tb") || !strings.Contains(reason, "below minimum") {
		t.Errorf("reason should explain the failed min bound, got %q", reason)
	}

	missing := baseItem()
	delete(missing.Attributes, "capacity_tb")
	ok, reason = f.Pass(missing)
	if ok {
		t.Fatalf("missing required attribute should fail")
	}
	if !strings.Contains(reason, "capacity_tb") || !strings.Contains(reason, "missing") {
		t.Errorf("reason should explain the missing attribute, got %q", reason)
	}

	unparseable := baseItem()
	unparseable.Attributes["capacity_tb"] = "sixteen"
	ok, reason = f.Pass(unparseable)
	if ok {
		t.Fatalf("unparseable attribute should fail")
	}
	if !strings.Contains(reason, "capacity_tb") || !strings.Contains(reason, "not numeric") {
		t.Errorf("reason should explain the unparseable attribute, got %q", reason)
	}
}

func TestFilterPass_MaxAttr(t *testing.T) {
	f := Filter{MaxAttr: map[string]float64{"capacity_tb": 20}}

	passing := baseItem() // 16 <= 20
	if ok, reason := f.Pass(passing); !ok {
		t.Fatalf("16 <= 20 should pass, got reason %q", reason)
	}

	tooBig := baseItem()
	tooBig.Attributes["capacity_tb"] = "24"
	ok, reason := f.Pass(tooBig)
	if ok {
		t.Fatalf("24 > 20 should fail")
	}
	if !strings.Contains(reason, "capacity_tb") || !strings.Contains(reason, "above maximum") {
		t.Errorf("reason should explain the failed max bound, got %q", reason)
	}

	missing := baseItem()
	delete(missing.Attributes, "capacity_tb")
	ok, reason = f.Pass(missing)
	if ok {
		t.Fatalf("missing attribute required by MaxAttr should fail")
	}
	if !strings.Contains(reason, "missing") {
		t.Errorf("reason should explain the missing attribute, got %q", reason)
	}
}

func TestFilterPass_AllowedConditions(t *testing.T) {
	f := Filter{AllowedConditions: []string{"new", "refurb"}}

	allowed := baseItem() // "refurb"
	if ok, reason := f.Pass(allowed); !ok {
		t.Fatalf("refurb should be allowed, got reason %q", reason)
	}

	disallowed := baseItem()
	disallowed.Condition = "used"
	ok, reason := f.Pass(disallowed)
	if ok {
		t.Fatalf("used should not be allowed")
	}
	if !strings.Contains(reason, "condition") {
		t.Errorf("reason should mention condition, got %q", reason)
	}
}

func TestFilterPass_HDDExampleFill(t *testing.T) {
	f := Filter{
		Category:      "hdd",
		RequirePriced: true,
		MinAttr:       map[string]float64{"capacity_tb": 8},
	}

	good := baseItem() // hdd, priced, 16TB
	if ok, reason := f.Pass(good); !ok {
		t.Fatalf("expected pass, got reason %q", reason)
	}

	small := baseItem()
	small.Attributes["capacity_tb"] = "2"
	if ok, _ := f.Pass(small); ok {
		t.Fatalf("2TB drive should fail an 8TB minimum")
	}
}

func TestFilterPass_PredicateOrder(t *testing.T) {
	// category should be checked (and fail) before other predicates, so the
	// reason names category, not some other unrelated failure.
	f := Filter{
		Category:      "hdd",
		RequirePriced: true,
	}
	it := item.Item{ID: "x", Category: "land", PriceCents: 0}
	ok, reason := f.Pass(it)
	if ok {
		t.Fatalf("expected failure")
	}
	if !strings.Contains(reason, "category") {
		t.Errorf("expected category to be the reported reason first, got %q", reason)
	}
}

// --- ScoreItem ---

func TestScoreItem_TierOrdering(t *testing.T) {
	it := baseItem()

	great := ScoreItem(it, DealSignal{Verdict: VerdictGreat, Ratio: 0.7, HasReference: true})
	good := ScoreItem(it, DealSignal{Verdict: VerdictGood, Ratio: 0.9, HasReference: true})
	market := ScoreItem(it, DealSignal{Verdict: VerdictMarket, Ratio: 1.0, HasReference: true})
	poor := ScoreItem(it, DealSignal{Verdict: VerdictPoor, Ratio: 1.3, HasReference: true})

	if !(great.Value > good.Value && good.Value > market.Value && market.Value > poor.Value) {
		t.Fatalf("expected great > good > market > poor, got great=%v good=%v market=%v poor=%v",
			great.Value, good.Value, market.Value, poor.Value)
	}

	if great.Tier != TierGreat || good.Tier != TierGood || market.Tier != TierMarket || poor.Tier != TierPoor {
		t.Errorf("unexpected tiers: great=%s good=%s market=%s poor=%s",
			great.Tier, good.Tier, market.Tier, poor.Tier)
	}
}

func TestScoreItem_UnknownVerdicts(t *testing.T) {
	it := baseItem()

	noRef := ScoreItem(it, DealSignal{Verdict: VerdictUnknownNoReference})
	if noRef.Tier != TierUnranked {
		t.Errorf("unknown-no-reference should be Tier unranked, got %s", noRef.Tier)
	}

	noPrice := ScoreItem(it, DealSignal{Verdict: VerdictUnknownNoPrice})
	if noPrice.Tier != TierUnranked {
		t.Errorf("unknown-no-price should be Tier unranked, got %s", noPrice.Tier)
	}

	// An unpriced item carries even less signal than an unvaluable-but-priced
	// one, so it should rank at or below unknown-no-reference.
	if noPrice.Value > noRef.Value {
		t.Errorf("expected unknown-no-price (%v) <= unknown-no-reference (%v)", noPrice.Value, noRef.Value)
	}

	// Both unknown verdicts must stay strictly below every known-verdict
	// tier's minimum plausible value, so unranked items never outrank a
	// ranked one.
	poor := ScoreItem(it, DealSignal{Verdict: VerdictPoor, Ratio: 5.0, HasReference: true})
	if noRef.Value >= poor.Value {
		t.Errorf("expected unranked (%v) < poor (%v)", noRef.Value, poor.Value)
	}
}

func TestScoreItem_UnrecognizedVerdict(t *testing.T) {
	it := baseItem()
	sc := ScoreItem(it, DealSignal{Verdict: "some-future-verdict", HasReference: true, Ratio: 0.1})
	if sc.Tier != TierUnranked {
		t.Errorf("unrecognized verdict should be treated as unranked, got %s", sc.Tier)
	}
	if !strings.Contains(sc.Rationale, "unrecognized") {
		t.Errorf("rationale should call out the unrecognized verdict, got %q", sc.Rationale)
	}
}

func TestScoreItem_RatioRefinesWithinTier(t *testing.T) {
	it := baseItem()

	cheaper := ScoreItem(it, DealSignal{Verdict: VerdictGood, Ratio: 0.80, HasReference: true})
	pricier := ScoreItem(it, DealSignal{Verdict: VerdictGood, Ratio: 0.95, HasReference: true})

	if cheaper.Tier != TierGood || pricier.Tier != TierGood {
		t.Fatalf("both should stay in tier good, got %s and %s", cheaper.Tier, pricier.Tier)
	}
	if !(cheaper.Value > pricier.Value) {
		t.Errorf("a lower ratio within the same tier should score higher: cheaper=%v pricier=%v",
			cheaper.Value, pricier.Value)
	}
}

func TestScoreItem_NoReferenceSkipsRefinement(t *testing.T) {
	it := baseItem()
	// HasReference false: Ratio must be ignored even if set to a misleading
	// value, so the score stays at tier base.
	sc := ScoreItem(it, DealSignal{Verdict: VerdictGood, Ratio: 0.01, HasReference: false})
	if sc.Value != baseGood {
		t.Errorf("expected unrefined base value %v when HasReference is false, got %v", baseGood, sc.Value)
	}
}

func TestScoreItem_RatioRefinementNeverCrossesTiers(t *testing.T) {
	it := baseItem()
	// Extreme ratios (best/worst case deltas) must not push a tier's value
	// across into a neighboring tier's base range, since Rank relies on tier
	// ordering being preserved by Value ordering.
	bestGood := ScoreItem(it, DealSignal{Verdict: VerdictGood, Ratio: -10, HasReference: true})
	worstGreat := ScoreItem(it, DealSignal{Verdict: VerdictGreat, Ratio: 10, HasReference: true})

	if bestGood.Value >= worstGreat.Value {
		t.Errorf("even best-refined good (%v) should stay below worst-refined great (%v)",
			bestGood.Value, worstGreat.Value)
	}
}

// --- Rank ---

func TestRank_OrdersByValueDescending(t *testing.T) {
	rs := []Ranked{
		{Item: item.Item{ID: "b"}, Score: Score{Value: 50}},
		{Item: item.Item{ID: "a"}, Score: Score{Value: 90}},
		{Item: item.Item{ID: "c"}, Score: Score{Value: 10}},
	}
	got := Rank(rs)
	want := []string{"a", "b", "c"}
	for i, id := range want {
		if got[i].Item.ID != id {
			t.Fatalf("position %d: expected %s, got %s", i, id, got[i].Item.ID)
		}
	}
}

func TestRank_DeterministicTiebreakByID(t *testing.T) {
	rs := []Ranked{
		{Item: item.Item{ID: "zzz"}, Score: Score{Value: 42}},
		{Item: item.Item{ID: "aaa"}, Score: Score{Value: 42}},
		{Item: item.Item{ID: "mmm"}, Score: Score{Value: 42}},
	}
	got := Rank(rs)
	want := []string{"aaa", "mmm", "zzz"}
	for i, id := range want {
		if got[i].Item.ID != id {
			t.Fatalf("position %d: expected %s, got %s", i, id, got[i].Item.ID)
		}
	}
}

func TestRank_DoesNotMutateInput(t *testing.T) {
	rs := []Ranked{
		{Item: item.Item{ID: "b"}, Score: Score{Value: 1}},
		{Item: item.Item{ID: "a"}, Score: Score{Value: 2}},
	}
	original := append([]Ranked(nil), rs...)
	_ = Rank(rs)
	for i := range rs {
		if rs[i].Item.ID != original[i].Item.ID {
			t.Fatalf("Rank mutated its input slice")
		}
	}
}

func TestRank_StableAcrossRepeatedCalls(t *testing.T) {
	rs := []Ranked{
		{Item: item.Item{ID: "x"}, Score: Score{Value: 5}},
		{Item: item.Item{ID: "y"}, Score: Score{Value: 5}},
	}
	first := Rank(rs)
	second := Rank(rs)
	for i := range first {
		if first[i].Item.ID != second[i].Item.ID {
			t.Fatalf("Rank is not deterministic across repeated calls")
		}
	}
}
