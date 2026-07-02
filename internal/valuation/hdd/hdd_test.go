package hdd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- fakeSource: an in-memory ReferenceSource for Valuer unit tests, so the
// Valuer's math/verdict logic is tested independent of any HTTP/vendor
// concern. ---

type fakeSource struct {
	fn func(ctx context.Context, capacityTB float64, condition string) (int64, bool, error)
}

func (f fakeSource) PricePerTB(ctx context.Context, capacityTB float64, condition string) (int64, bool, error) {
	return f.fn(ctx, capacityTB, condition)
}

func TestValuer_VerdictTiers(t *testing.T) {
	// Reference is fixed at 1000 cents/TB for refurb 16TB throughout.
	const refCentsPerTB = 1000
	src := fakeSource{fn: func(_ context.Context, capacityTB float64, condition string) (int64, bool, error) {
		if capacityTB == 16 && condition == ConditionRefurb {
			return refCentsPerTB, true, nil
		}
		return 0, false, nil
	}}
	v := Valuer{Source: src}

	cases := []struct {
		name       string
		priceCents int64 // for 16TB
		wantRatio  float64
		want       Verdict
	}{
		{"great: 60% of reference", 16 * 600, 0.60, VerdictGreat},
		{"great boundary: exactly 0.80", 16 * 800, 0.80, VerdictGreat},
		{"good: just above great boundary", 16 * 810, 0.81, VerdictGood},
		{"good boundary: exactly 0.95", 16 * 950, 0.95, VerdictGood},
		{"market: just above good boundary", 16 * 960, 0.96, VerdictMarket},
		{"market boundary: exactly 1.10", 16 * 1100, 1.10, VerdictMarket},
		{"poor: just above market boundary", 16 * 1110, 1.11, VerdictPoor},
		{"poor: well above reference", 16 * 2000, 2.00, VerdictPoor},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v.Value(context.Background(), 16, tc.priceCents, ConditionRefurb)
			if err != nil {
				t.Fatalf("Value: unexpected error: %v", err)
			}
			if got.Verdict != tc.want {
				t.Errorf("Verdict = %q, want %q (ratio=%v)", got.Verdict, tc.want, got.Ratio)
			}
			if !got.ReferenceAvailable {
				t.Fatalf("ReferenceAvailable = false, want true")
			}
			if got.ReferenceCentsPerTB != refCentsPerTB {
				t.Errorf("ReferenceCentsPerTB = %d, want %d", got.ReferenceCentsPerTB, refCentsPerTB)
			}
			if diff := got.Ratio - tc.wantRatio; diff > 0.0001 || diff < -0.0001 {
				t.Errorf("Ratio = %v, want ~%v", got.Ratio, tc.wantRatio)
			}
			wantListingCentsPerTB := tc.priceCents / 16
			if got.ListingCentsPerTB != wantListingCentsPerTB {
				t.Errorf("ListingCentsPerTB = %d, want %d", got.ListingCentsPerTB, wantListingCentsPerTB)
			}
		})
	}
}

func TestValuer_UnknownNoPrice(t *testing.T) {
	src := fakeSource{fn: func(context.Context, float64, string) (int64, bool, error) {
		t.Fatalf("source should not be consulted when price is unknown (0)")
		return 0, false, nil
	}}
	v := Valuer{Source: src}

	got, err := v.Value(context.Background(), 16, 0, ConditionRefurb)
	if err != nil {
		t.Fatalf("Value: unexpected error: %v", err)
	}
	if got.Verdict != VerdictUnknownNoPrice {
		t.Errorf("Verdict = %q, want %q", got.Verdict, VerdictUnknownNoPrice)
	}
	if got.ReferenceAvailable {
		t.Errorf("ReferenceAvailable = true, want false")
	}
	if got.Ratio != 0 {
		t.Errorf("Ratio = %v, want 0 (never divide on unknown price)", got.Ratio)
	}
}

func TestValuer_UnknownNoReference(t *testing.T) {
	src := fakeSource{fn: func(context.Context, float64, string) (int64, bool, error) {
		return 0, false, nil // no data for anything, direct or refurb anchor
	}}
	v := Valuer{Source: src}

	got, err := v.Value(context.Background(), 16, 16*1000, ConditionNew)
	if err != nil {
		t.Fatalf("Value: unexpected error: %v", err)
	}
	if got.Verdict != VerdictUnknownNoReference {
		t.Errorf("Verdict = %q, want %q", got.Verdict, VerdictUnknownNoReference)
	}
	if got.ReferenceAvailable {
		t.Errorf("ReferenceAvailable = true, want false")
	}
	// ListingCentsPerTB is still computed -- we know the listing's own $/TB
	// even without a reference to compare it to.
	if got.ListingCentsPerTB != 1000 {
		t.Errorf("ListingCentsPerTB = %d, want 1000", got.ListingCentsPerTB)
	}
}

func TestValuer_ErrorOnNonPositiveCapacity(t *testing.T) {
	src := fakeSource{fn: func(context.Context, float64, string) (int64, bool, error) {
		t.Fatalf("source should not be consulted when capacity is invalid")
		return 0, false, nil
	}}
	v := Valuer{Source: src}

	for _, cap := range []float64{0, -1, -0.5} {
		_, err := v.Value(context.Background(), cap, 1000, ConditionRefurb)
		if !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("capacityTB=%v: err = %v, want ErrInvalidCapacity", cap, err)
		}
	}
}

func TestValuer_ErrorOnNegativePrice(t *testing.T) {
	src := fakeSource{fn: func(context.Context, float64, string) (int64, bool, error) {
		t.Fatalf("source should not be consulted when price is invalid")
		return 0, false, nil
	}}
	v := Valuer{Source: src}

	_, err := v.Value(context.Background(), 16, -100, ConditionRefurb)
	if err == nil {
		t.Fatalf("expected an error for negative price_cents, got nil")
	}
}

func TestValuer_SourceErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom")
	src := fakeSource{fn: func(context.Context, float64, string) (int64, bool, error) {
		return 0, false, wantErr
	}}
	v := Valuer{Source: src}

	_, err := v.Value(context.Background(), 16, 1000, ConditionRefurb)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", err, wantErr)
	}
}

// TestValuer_ConditionFallbackDerivesFromRefurbAnchor exercises the
// documented fallback: when the source has no direct "new" (or "used") data
// but does have "refurb" data, Valuer derives an estimated reference by
// applying conditionMultiplier to the refurb anchor, and marks the result as
// derived so callers can distinguish it from a direct source hit.
func TestValuer_ConditionFallbackDerivesFromRefurbAnchor(t *testing.T) {
	const refurbAnchor = 1000
	src := fakeSource{fn: func(_ context.Context, capacityTB float64, condition string) (int64, bool, error) {
		if capacityTB == 16 && condition == ConditionRefurb {
			return refurbAnchor, true, nil
		}
		return 0, false, nil // "new" and "used" direct lookups miss
	}}
	v := Valuer{Source: src}

	t.Run("new tier derived above refurb anchor", func(t *testing.T) {
		got, err := v.Value(context.Background(), 16, 16*1000, ConditionNew)
		if err != nil {
			t.Fatalf("Value: unexpected error: %v", err)
		}
		if !got.ReferenceAvailable {
			t.Fatalf("ReferenceAvailable = false, want true (fallback should have resolved)")
		}
		if !got.ReferenceDerived {
			t.Errorf("ReferenceDerived = false, want true")
		}
		wantRef := int64(float64(refurbAnchor) * conditionMultiplier[ConditionNew])
		if got.ReferenceCentsPerTB != wantRef {
			t.Errorf("ReferenceCentsPerTB = %d, want %d (refurb anchor * new multiplier)", got.ReferenceCentsPerTB, wantRef)
		}
		if got.ReferenceCentsPerTB <= refurbAnchor {
			t.Errorf("derived new reference (%d) should be above the refurb anchor (%d)", got.ReferenceCentsPerTB, refurbAnchor)
		}
	})

	t.Run("used tier derived below refurb anchor", func(t *testing.T) {
		got, err := v.Value(context.Background(), 16, 16*1000, ConditionUsed)
		if err != nil {
			t.Fatalf("Value: unexpected error: %v", err)
		}
		if !got.ReferenceAvailable {
			t.Fatalf("ReferenceAvailable = false, want true (fallback should have resolved)")
		}
		if !got.ReferenceDerived {
			t.Errorf("ReferenceDerived = false, want true")
		}
		wantRef := int64(float64(refurbAnchor) * conditionMultiplier[ConditionUsed])
		if got.ReferenceCentsPerTB != wantRef {
			t.Errorf("ReferenceCentsPerTB = %d, want %d (refurb anchor * used multiplier)", got.ReferenceCentsPerTB, wantRef)
		}
		if got.ReferenceCentsPerTB >= refurbAnchor {
			t.Errorf("derived used reference (%d) should be below the refurb anchor (%d)", got.ReferenceCentsPerTB, refurbAnchor)
		}
	})

	t.Run("direct hit is not marked derived", func(t *testing.T) {
		got, err := v.Value(context.Background(), 16, 16*1000, ConditionRefurb)
		if err != nil {
			t.Fatalf("Value: unexpected error: %v", err)
		}
		if got.ReferenceDerived {
			t.Errorf("ReferenceDerived = true, want false for a direct source hit")
		}
	})

	t.Run("no refurb anchor either -> unknown-no-reference", func(t *testing.T) {
		emptySrc := fakeSource{fn: func(context.Context, float64, string) (int64, bool, error) {
			return 0, false, nil
		}}
		v2 := Valuer{Source: emptySrc}
		got, err := v2.Value(context.Background(), 16, 16*1000, ConditionNew)
		if err != nil {
			t.Fatalf("Value: unexpected error: %v", err)
		}
		if got.Verdict != VerdictUnknownNoReference {
			t.Errorf("Verdict = %q, want %q", got.Verdict, VerdictUnknownNoReference)
		}
	})

	t.Run("unrecognized condition string with no direct hit -> unknown-no-reference", func(t *testing.T) {
		got, err := v.Value(context.Background(), 16, 16*1000, "frankenstein")
		if err != nil {
			t.Fatalf("Value: unexpected error: %v", err)
		}
		if got.Verdict != VerdictUnknownNoReference {
			t.Errorf("Verdict = %q, want %q (no multiplier known for this condition)", got.Verdict, VerdictUnknownNoReference)
		}
	})
}

func TestValuer_CustomThresholds(t *testing.T) {
	src := fakeSource{fn: func(_ context.Context, capacityTB float64, condition string) (int64, bool, error) {
		return 1000, true, nil
	}}
	v := Valuer{Source: src, GreatMaxRatio: 0.5, GoodMaxRatio: 0.6, MarketMaxRatio: 0.7}

	// ratio 0.55 would be "great" under defaults but is "good" under the
	// custom thresholds above.
	got, err := v.Value(context.Background(), 16, int64(16*550), ConditionRefurb)
	if err != nil {
		t.Fatalf("Value: unexpected error: %v", err)
	}
	if got.Verdict != VerdictGood {
		t.Errorf("Verdict = %q, want %q under custom thresholds", got.Verdict, VerdictGood)
	}
}

// --- ShopifySource tests: canned products.json served over httptest, never
// the real network. ---

const cannedShopifyJSON = `{
  "products": [
    {
      "title": "16TB Seagate Exos X16 SAS 12Gb/s - Manufacturer Recertified",
      "product_type": "Hard Drives",
      "tags": ["Recertified", "3.5-inch", "SAS"],
      "variants": [
        {"title": "16TB / SAS", "price": "179.99", "available": true, "sku": "ST16000-A"},
        {"title": "16TB / SAS", "price": "184.99", "available": true, "sku": "ST16000-B"}
      ]
    },
    {
      "title": "16TB WD Ultrastar SATA - Manufacturer Recertified",
      "product_type": "Hard Drives",
      "tags": "Recertified,3.5-inch,SATA",
      "variants": [
        {"title": "16TB / SATA", "price": "174.99", "available": true, "sku": "WD16000-A"}
      ]
    },
    {
      "title": "8TB Seagate Exos X8 SAS - New Retail",
      "product_type": "Hard Drives",
      "tags": ["New"],
      "variants": [
        {"title": "8TB / SAS", "price": "159.99", "available": true, "sku": "ST8000-NEW"}
      ]
    },
    {
      "title": "12TB Seagate Exos X12 SAS - Manufacturer Recertified",
      "product_type": "Hard Drives",
      "tags": ["Recertified"],
      "variants": [
        {"title": "12TB / SAS", "price": "129.99", "available": false, "sku": "ST12000-SOLDOUT"}
      ]
    },
    {
      "title": "Random accessory with no capacity in the title",
      "product_type": "Accessories",
      "tags": [],
      "variants": [
        {"title": "Default", "price": "9.99", "available": true, "sku": "ACC-1"}
      ]
    }
  ]
}`

func newTestShopifyServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestShopifySource_PricePerTB_MatchesCapacityAndCondition(t *testing.T) {
	srv := newTestShopifyServer(t, cannedShopifyJSON, http.StatusOK)
	src := &ShopifySource{ProductsURL: srv.URL, HTTPClient: srv.Client()}

	// Two 16TB refurb variants from product 1 (179.99, 184.99) plus one from
	// product 2 (174.99) -> three offers at capacity 16, condition refurb.
	// $/TB = price/16; median of the three.
	got, ok, err := src.PricePerTB(context.Background(), 16, ConditionRefurb)
	if err != nil {
		t.Fatalf("PricePerTB: unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("PricePerTB: ok = false, want true")
	}
	// Per-offer $/TB in cents: 174.99/16 -> 1094, 179.99/16 -> 1125,
	// 184.99/16 -> 1156 (each rounded). Median of the three sorted is 1125.
	const want = 1125
	if got != want {
		t.Errorf("PricePerTB = %d cents/TB, want %d", got, want)
	}
}

func TestShopifySource_PricePerTB_NewCondition(t *testing.T) {
	srv := newTestShopifyServer(t, cannedShopifyJSON, http.StatusOK)
	src := &ShopifySource{ProductsURL: srv.URL, HTTPClient: srv.Client()}

	got, ok, err := src.PricePerTB(context.Background(), 8, ConditionNew)
	if err != nil {
		t.Fatalf("PricePerTB: unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("PricePerTB: ok = false, want true for the 8TB New Retail product")
	}
	// 159.99*100/8 = 1999.875 -> rounds to 2000.
	const want = 2000
	if got != want {
		t.Errorf("PricePerTB = %d, want %d", got, want)
	}
}

func TestShopifySource_PricePerTB_UnavailableVariantExcluded(t *testing.T) {
	srv := newTestShopifyServer(t, cannedShopifyJSON, http.StatusOK)
	src := &ShopifySource{ProductsURL: srv.URL, HTTPClient: srv.Client()}

	// The only 12TB product's sole variant is available:false, so it must
	// not be counted.
	_, ok, err := src.PricePerTB(context.Background(), 12, ConditionRefurb)
	if err != nil {
		t.Fatalf("PricePerTB: unexpected error: %v", err)
	}
	if ok {
		t.Errorf("PricePerTB: ok = true, want false (only matching variant is unavailable)")
	}
}

func TestShopifySource_PricePerTB_NoMatchingCapacity(t *testing.T) {
	srv := newTestShopifyServer(t, cannedShopifyJSON, http.StatusOK)
	src := &ShopifySource{ProductsURL: srv.URL, HTTPClient: srv.Client()}

	_, ok, err := src.PricePerTB(context.Background(), 22, ConditionRefurb)
	if err != nil {
		t.Fatalf("PricePerTB: unexpected error: %v", err)
	}
	if ok {
		t.Errorf("PricePerTB: ok = true, want false (no 22TB offers in the canned feed)")
	}
}

func TestShopifySource_PricePerTB_NonOKStatus(t *testing.T) {
	srv := newTestShopifyServer(t, `{"errors":"nope"}`, http.StatusInternalServerError)
	src := &ShopifySource{ProductsURL: srv.URL, HTTPClient: srv.Client()}

	_, _, err := src.PricePerTB(context.Background(), 16, ConditionRefurb)
	if err == nil {
		t.Fatalf("expected an error for a non-200 response, got nil")
	}
}

func TestShopifySource_PricePerTB_MalformedJSON(t *testing.T) {
	srv := newTestShopifyServer(t, `{not valid json`, http.StatusOK)
	src := &ShopifySource{ProductsURL: srv.URL, HTTPClient: srv.Client()}

	_, _, err := src.PricePerTB(context.Background(), 16, ConditionRefurb)
	if err == nil {
		t.Fatalf("expected an error for malformed JSON, got nil")
	}
}

func TestShopifySource_PricePerTB_InvalidCapacity(t *testing.T) {
	srv := newTestShopifyServer(t, cannedShopifyJSON, http.StatusOK)
	src := &ShopifySource{ProductsURL: srv.URL, HTTPClient: srv.Client()}

	_, _, err := src.PricePerTB(context.Background(), 0, ConditionRefurb)
	if !errors.Is(err, ErrInvalidCapacity) {
		t.Errorf("err = %v, want ErrInvalidCapacity", err)
	}
}

func TestShopifySource_CachesWithinTTL(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedShopifyJSON))
	}))
	t.Cleanup(srv.Close)

	src := &ShopifySource{ProductsURL: srv.URL, HTTPClient: srv.Client(), CacheTTL: time.Second}

	if _, _, err := src.PricePerTB(context.Background(), 16, ConditionRefurb); err != nil {
		t.Fatalf("PricePerTB #1: %v", err)
	}
	if _, _, err := src.PricePerTB(context.Background(), 8, ConditionNew); err != nil {
		t.Fatalf("PricePerTB #2: %v", err)
	}
	if calls != 1 {
		t.Errorf("HTTP calls = %d, want 1 (second lookup should hit the cache)", calls)
	}
}

// --- pure helper function tests ---

func TestParseCapacityTB(t *testing.T) {
	cases := []struct {
		in     string
		wantTB float64
		wantOK bool
	}{
		{"16TB Seagate Exos", 16, true},
		{"14 TB WD Ultrastar", 14, true},
		{"1.5TB laptop drive", 1.5, true},
		{"no capacity here", 0, false},
		{"TB without a number", 0, false},
	}
	for _, tc := range cases {
		gotTB, gotOK := parseCapacityTB(tc.in)
		if gotOK != tc.wantOK || (gotOK && gotTB != tc.wantTB) {
			t.Errorf("parseCapacityTB(%q) = (%v, %v), want (%v, %v)", tc.in, gotTB, gotOK, tc.wantTB, tc.wantOK)
		}
	}
}

func TestInferCondition(t *testing.T) {
	cases := []struct {
		title string
		tags  []string
		want  string
	}{
		{"16TB - Manufacturer Recertified", nil, ConditionRefurb},
		{"16TB drive", []string{"Refurb"}, ConditionRefurb},
		{"8TB - New Retail", nil, ConditionNew},
		{"used pull drive", nil, ConditionUsed},
		{"no signal at all", nil, ConditionRefurb}, // default anchor per doc
	}
	for _, tc := range cases {
		got := inferCondition(tc.title, tc.tags)
		if got != tc.want {
			t.Errorf("inferCondition(%q, %v) = %q, want %q", tc.title, tc.tags, got, tc.want)
		}
	}
}

func TestShopifyTagsUnmarshal_ArrayAndString(t *testing.T) {
	var arr shopifyTags
	if err := json.Unmarshal([]byte(`["A","B"]`), &arr); err != nil {
		t.Fatalf("array form: %v", err)
	}
	if len(arr) != 2 || arr[0] != "A" || arr[1] != "B" {
		t.Errorf("array form = %v, want [A B]", arr)
	}

	var str shopifyTags
	if err := json.Unmarshal([]byte(`"A, B"`), &str); err != nil {
		t.Fatalf("string form: %v", err)
	}
	if len(str) != 2 || str[0] != "A" || str[1] != "B" {
		t.Errorf("string form = %v, want [A B]", str)
	}
}

func TestMedian(t *testing.T) {
	if got := median([]int64{5}); got != 5 {
		t.Errorf("median([5]) = %d, want 5", got)
	}
	if got := median([]int64{1, 3}); got != 2 {
		t.Errorf("median([1,3]) = %d, want 2", got)
	}
	if got := median([]int64{3, 1, 2}); got != 2 {
		t.Errorf("median([3,1,2]) = %d, want 2", got)
	}
}
