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
