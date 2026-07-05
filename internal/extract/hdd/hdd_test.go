package hdd

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
)

func TestCategory(t *testing.T) {
	if got := New().Category(); got != "hdd" {
		t.Fatalf("Category() = %q, want %q", got, "hdd")
	}
}

func TestExtractCapacityTB(t *testing.T) {
	cases := []struct {
		name    string
		title   string
		wantTB  string
		wantSet bool
	}{
		{"bare TB adjacent to text", "Seagate Exos X18 16TB SAS", "16", true},
		{"spaced TB", "16 TB Enterprise Drive", "16", true},
		{"GB converts to TB", "8000GB Enterprise HDD", "8", true},
		{"decimal TB drops trailing zero", "14.0TB Helium Drive", "14", true},
		{"spaced GB converts to TB", "8000 GB Enterprise HDD", "8", true},
		{"no capacity present", "Seagate Exos X18 Enterprise SAS Drive", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractCapacityTB(tc.title)
			if ok != tc.wantSet {
				t.Fatalf("extractCapacityTB(%q) ok = %v, want %v", tc.title, ok, tc.wantSet)
			}
			if got != tc.wantTB {
				t.Fatalf("extractCapacityTB(%q) = %q, want %q", tc.title, got, tc.wantTB)
			}
		})
	}
}

func TestExtractCapacityTB_AvoidsBitRateFalsePositive(t *testing.T) {
	// "6Gb/s" is a SATA link speed in bits, not a capacity in bytes; it must
	// not be picked up as capacity_tb. No other capacity is present, so this
	// must report absent.
	_, ok := extractCapacityTB("Seagate 6Gb/s SATA Enterprise Drive")
	if ok {
		t.Fatalf("extractCapacityTB matched a bit-rate spec as capacity")
	}
}

func TestExtract_CapacityAttribute(t *testing.T) {
	e := New()
	s := baseSanitized()
	s.Title = "Seagate Exos X18 16TB SAS Enterprise Hard Drive"
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if got := it.Attributes["capacity_tb"]; got != "16" {
		t.Fatalf("Attributes[capacity_tb] = %q, want %q", got, "16")
	}

	s2 := baseSanitized()
	s2.Title = "Seagate Exos X18 Enterprise Hard Drive"
	it2, err := e.Extract(context.Background(), s2)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if _, ok := it2.Attributes["capacity_tb"]; ok {
		t.Fatalf("Attributes[capacity_tb] present = %q, want absent", it2.Attributes["capacity_tb"])
	}
}

func TestExtractCondition_FromConditionID(t *testing.T) {
	cases := []struct {
		conditionID string
		want        string
	}{
		{"1000", "new"},
		{"1500", "new"},
		{"2000", "refurb"},
		{"2010", "refurb"},
		{"2500", "refurb"},
		{"3000", "used"},
		{"7000", "parts"},
	}
	for _, tc := range cases {
		t.Run(tc.conditionID, func(t *testing.T) {
			got := extractCondition(tc.conditionID, "some drive with no keywords")
			if got != tc.want {
				t.Fatalf("extractCondition(%q, ...) = %q, want %q", tc.conditionID, got, tc.want)
			}
		})
	}
}

func TestExtractCondition_KeywordFallback(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  string
	}{
		{"renewed", "Seagate 16TB Renewed Hard Drive", "refurb"},
		{"refurbished", "WD 14TB Refurbished Enterprise Drive", "refurb"},
		{"recertified", "Seagate 12TB Recertified Enterprise Drive", "refurb"},
		{"manufacturer recertified", "Seagate 12TB Manufacturer Recertified Drive", "refurb"},
		{"brand new", "Brand New Seagate 16TB Drive", "new"},
		{"new", "New Seagate 16TB Drive", "new"},
		{"used", "Used Seagate 16TB Drive", "used"},
		{"for parts", "Seagate 16TB Drive For Parts Not Working", "parts"},
		{"as-is", "Seagate 16TB Drive Sold As-Is", "parts"},
		{"no keywords at all", "Seagate 16TB SAS Enterprise Drive", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCondition("", tc.title)
			if got != tc.want {
				t.Fatalf("extractCondition(\"\", %q) = %q, want %q", tc.title, got, tc.want)
			}
		})
	}
}

func TestExtractCondition_UnrecognizedConditionIDFallsBackToTitle(t *testing.T) {
	got := extractCondition("9999", "Seagate 16TB Refurbished Drive")
	if got != "refurb" {
		t.Fatalf("extractCondition(unknown id, ...) = %q, want %q", got, "refurb")
	}
}

func TestDeterministicID(t *testing.T) {
	e := New()
	s1 := baseSanitized()
	s1.SourceID = "ebay"
	s1.SourceKey = "12345"

	it1a, err := e.Extract(context.Background(), s1)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	it1b, err := e.Extract(context.Background(), s1)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if it1a.ID != it1b.ID {
		t.Fatalf("same SourceID+SourceKey produced different ids: %q vs %q", it1a.ID, it1b.ID)
	}
	if it1a.ID == "" {
		t.Fatalf("ID is empty")
	}

	s2 := baseSanitized()
	s2.SourceID = "ebay"
	s2.SourceKey = "67890"
	it2, err := e.Extract(context.Background(), s2)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if it1a.ID == it2.ID {
		t.Fatalf("different SourceKey produced the same id: %q", it1a.ID)
	}
}

func TestTokenize(t *testing.T) {
	got := tokenize("Seagate 16TB 16TB Exos-X18 SAS drive!")
	want := []string{"seagate", "16tb", "exos", "x18", "sas", "drive"}
	if len(got) != len(want) {
		t.Fatalf("tokenize() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokenize()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestTokenize_DropsSingleCharTokens(t *testing.T) {
	got := tokenize("a 16TB b drive")
	for _, tok := range got {
		if len(tok) < 2 {
			t.Fatalf("tokenize() left a single-char token %q in %v", tok, got)
		}
	}
}

func TestExtract_WellFormedPassesValidate(t *testing.T) {
	e := New()
	s := baseSanitized()
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if err := it.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if it.Class != item.ClassDurable {
		t.Fatalf("Class = %q, want %q", it.Class, item.ClassDurable)
	}
	if it.Title != s.Title {
		t.Fatalf("Title = %q, want verbatim %q", it.Title, s.Title)
	}
	if it.Category != "hdd" {
		t.Fatalf("Category = %q, want %q", it.Category, "hdd")
	}
}

func TestExtract_EmptySourceKeyErrors(t *testing.T) {
	e := New()
	s := baseSanitized()
	s.SourceKey = ""
	_, err := e.Extract(context.Background(), s)
	if err == nil {
		t.Fatalf("Extract() error = nil, want error for empty SourceKey")
	}
}

func TestExtract_NegativePriceErrors(t *testing.T) {
	e := New()
	s := baseSanitized()
	s.PriceCents = -100
	_, err := e.Extract(context.Background(), s)
	if err == nil {
		t.Fatalf("Extract() error = nil, want error for negative PriceCents")
	}
}

func TestExtract_CanonicalIDBestEffort(t *testing.T) {
	e := New()

	s := baseSanitized()
	s.Title = "Seagate ST16000NM001G 16TB Enterprise Exos X18 SAS"
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if it.CanonicalID != "ST16000NM001G" {
		t.Fatalf("CanonicalID = %q, want %q", it.CanonicalID, "ST16000NM001G")
	}

	s2 := baseSanitized()
	s2.Title = "Generic 16TB Enterprise Hard Drive"
	it2, err := e.Extract(context.Background(), s2)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if it2.CanonicalID != "" {
		t.Fatalf("CanonicalID = %q, want empty", it2.CanonicalID)
	}
}

func TestSellerFeedbackTier(t *testing.T) {
	cases := []struct {
		pct     string
		want    string
		wantSet bool
	}{
		{"100.0", "high", true},
		{"99.4", "high", true},
		{"99.0", "high", true},   // lower boundary of high
		{"98.99", "good", true},  // just under high
		{"97.0", "good", true},   // lower boundary of good
		{"96.99", "mixed", true}, // just under good
		{"90.0", "mixed", true},  // lower boundary of mixed
		{"89.9", "low", true},
		{"0.0", "low", true},
		{"", "", false},        // absent -> no attribute
		{"garbage", "", false}, // unparseable -> no attribute
	}
	for _, tc := range cases {
		t.Run(tc.pct, func(t *testing.T) {
			got, ok := sellerFeedbackTier(tc.pct)
			if ok != tc.wantSet {
				t.Fatalf("sellerFeedbackTier(%q) ok = %v, want %v", tc.pct, ok, tc.wantSet)
			}
			if got != tc.want {
				t.Fatalf("sellerFeedbackTier(%q) = %q, want %q", tc.pct, got, tc.want)
			}
		})
	}
}

func TestSellerVolumeTier(t *testing.T) {
	cases := []struct {
		score   string
		want    string
		wantSet bool
	}{
		{"5000", "established", true},
		{"1000", "established", true}, // lower boundary
		{"999", "mid", true},
		{"100", "mid", true}, // lower boundary
		{"99", "new", true},
		{"0", "new", true}, // brand-new seller, score present
		{"", "", false},    // absent
		{"abc", "", false}, // unparseable
	}
	for _, tc := range cases {
		t.Run(tc.score, func(t *testing.T) {
			got, ok := sellerVolumeTier(tc.score)
			if ok != tc.wantSet {
				t.Fatalf("sellerVolumeTier(%q) ok = %v, want %v", tc.score, ok, tc.wantSet)
			}
			if got != tc.want {
				t.Fatalf("sellerVolumeTier(%q) = %q, want %q", tc.score, got, tc.want)
			}
		})
	}
}

func TestShipsFromUS(t *testing.T) {
	cases := []struct {
		country string
		want    string
		wantSet bool
	}{
		{"US", "true", true},
		{"CA", "false", true},
		{"GB", "false", true},
		{"", "", false}, // absent -> no attribute (not "false")
	}
	for _, tc := range cases {
		t.Run(tc.country, func(t *testing.T) {
			got, ok := shipsFromUS(tc.country)
			if ok != tc.wantSet {
				t.Fatalf("shipsFromUS(%q) ok = %v, want %v", tc.country, ok, tc.wantSet)
			}
			if got != tc.want {
				t.Fatalf("shipsFromUS(%q) = %q, want %q", tc.country, got, tc.want)
			}
		})
	}
}

func TestExtract_SellerAttributes(t *testing.T) {
	e := New()
	s := baseSanitized()
	s.Aspects = map[string]string{
		"seller_feedback_pct":   "99.4",
		"seller_feedback_score": "1500",
		"item_location_country": "US",
	}
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	want := map[string]string{
		"seller_feedback_tier": "high",
		"seller_volume_tier":   "established",
		"ships_from_us":        "true",
	}
	for k, v := range want {
		if got := it.Attributes[k]; got != v {
			t.Fatalf("Attributes[%q] = %q, want %q", k, got, v)
		}
	}

	// Absent seller aspects -> the seller attributes are omitted entirely, not
	// stored as "unknown"/"false" (absence is a distinct, non-identifying fact).
	s2 := baseSanitized()
	it2, err := e.Extract(context.Background(), s2)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	for _, k := range []string{"seller_feedback_tier", "seller_volume_tier", "ships_from_us"} {
		if _, ok := it2.Attributes[k]; ok {
			t.Fatalf("Attributes[%q] present = %q, want absent", k, it2.Attributes[k])
		}
	}
}

// TestExtract_NeverStoresSellerIdentity is the compliance guard behind the eBay
// account-deletion OPT-OUT: even if a connector leaks a seller username (or a
// raw feedback value) into Aspects, the extracted item -- the only thing that is
// persisted -- must carry NO seller identifier and NO raw seller number, only
// coarse buckets. If this test ever fails, the "we store no eBay user PII"
// attestation is no longer truthful.
func TestExtract_NeverStoresSellerIdentity(t *testing.T) {
	e := New()
	s := baseSanitized()
	const username = "secretSeller99"
	s.Aspects = map[string]string{
		"seller_username":       username,
		"seller_feedback_pct":   "99.4",
		"seller_feedback_score": "1500",
		"item_location_country": "US",
	}
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	// The username must not survive into any persisted field.
	for k, v := range it.Attributes {
		if v == username || k == "seller_username" {
			t.Fatalf("username leaked into Attributes[%q] = %q", k, v)
		}
	}
	for _, tok := range it.Tokens {
		if tok == username {
			t.Fatalf("username leaked into Tokens")
		}
	}
	// Raw feedback numbers must be bucketed, never stored verbatim.
	for k, v := range it.Attributes {
		if v == "99.4" || v == "1500" {
			t.Fatalf("raw seller value stored in Attributes[%q] = %q (must be a coarse tier)", k, v)
		}
	}
}

// baseSanitized returns a minimal, well-formed listing.Sanitized suitable as
// a starting point for test cases that only need to vary one field.
func baseSanitized() listing.Sanitized {
	return listing.Sanitized{
		SourceID:     "ebay",
		SourceKey:    "v1|123456789012|0",
		SourceURL:    "https://example.invalid/itm/123456789012",
		Title:        "Seagate Exos X18 16TB SAS Enterprise Hard Drive",
		Body:         "",
		PriceCents:   24999,
		Currency:     "USD",
		ConditionRaw: "2500",
		Aspects:      map[string]string{},
		SeenAt:       time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		Boundary:     "test-sanitizer",
	}
}
