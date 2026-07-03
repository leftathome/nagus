package land

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
)

func TestCategory(t *testing.T) {
	if got := New().Category(); got != "land" {
		t.Fatalf("Category() = %q, want %q", got, "land")
	}
}

func TestExtractAcreage(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		wantAcres string
		wantSet   bool
	}{
		{"acres with well", "5 Acres with Well", "5", true},
		{"decimal acre singular", "5.5 acre parcel", "5.5", true},
		{"ac abbreviation", "10 ac lot", "10", true},
		{"sq ft exact acre", "43560 sq ft", "1", true},
		{"sqft no space no period", "21780 sqft lot", "0.5", true},
		{"square feet spelled out", "87120 square feet of land", "2", true},
		{"comma separated sqft", "1,000,000 sq ft parcel", "22.96", true},
		{"no acreage present", "Beautiful lot ready to build", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractAcreage(tc.text)
			if ok != tc.wantSet {
				t.Fatalf("extractAcreage(%q) ok = %v, want %v", tc.text, ok, tc.wantSet)
			}
			if got != tc.wantAcres {
				t.Fatalf("extractAcreage(%q) = %q, want %q", tc.text, got, tc.wantAcres)
			}
		})
	}
}

func TestExtractAcreage_PrefersAcresOverSqft(t *testing.T) {
	// A listing that mentions both an explicit acreage and a sqft figure
	// (e.g. describing a structure's square footage) must use the acreage.
	got, ok := extractAcreage("5 acres of land with a 2000 sq ft barn")
	if !ok {
		t.Fatalf("extractAcreage() ok = false, want true")
	}
	if got != "5" {
		t.Fatalf("extractAcreage() = %q, want %q (acres should win over sqft)", got, "5")
	}
}

func TestExtract_AcreageAttribute(t *testing.T) {
	e := New()
	s := baseSanitized()
	s.Title = "5 Acres with Well"
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if got := it.Attributes["acreage"]; got != "5" {
		t.Fatalf("Attributes[acreage] = %q, want %q", got, "5")
	}

	s2 := baseSanitized()
	s2.Title = "Nice flat lot, build your dream home"
	it2, err := e.Extract(context.Background(), s2)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if got, ok := it2.Attributes["acreage"]; ok {
		t.Fatalf("Attributes[acreage] present = %q, want absent", got)
	}
}

func TestExtract_Flags(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[string]string
	}{
		{
			name: "well and septic",
			body: "This parcel has well and septic already installed",
			want: map[string]string{"well": "true", "septic": "true"},
		},
		{
			name: "fixer-upper",
			body: "Fixer-upper cabin on the land",
			want: map[string]string{"fixer": "true"},
		},
		{
			name: "teardown",
			body: "Teardown house on a large lot",
			want: map[string]string{"fixer": "true"},
		},
		{
			name: "as-is",
			body: "Sold as-is, no warranties",
			want: map[string]string{"fixer": "true"},
		},
		{
			name: "manufactured home",
			body: "Includes a manufactured home",
			want: map[string]string{"manufactured": "true"},
		},
		{
			name: "mobile home",
			body: "Has an older mobile home on site",
			want: map[string]string{"manufactured": "true"},
		},
		{
			name: "owner financing",
			body: "Owner financing available, call for terms",
			want: map[string]string{"owner_financing": "true"},
		},
		{
			name: "seller financed",
			body: "Seller financing possible with 20% down",
			want: map[string]string{"owner_financing": "true"},
		},
		{
			name: "clean listing sets none",
			body: "Gorgeous vacant lot near the lake, great views",
			want: map[string]string{},
		},
	}

	e := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSanitized()
			s.Body = tc.body
			it, err := e.Extract(context.Background(), s)
			if err != nil {
				t.Fatalf("Extract() error = %v", err)
			}
			for _, attr := range []string{"well", "septic", "fixer", "manufactured", "owner_financing"} {
				want, wantSet := tc.want[attr]
				got, gotSet := it.Attributes[attr]
				if wantSet != gotSet || (wantSet && got != want) {
					t.Fatalf("Attributes[%q] = (%q, %v), want (%q, %v)", attr, got, gotSet, want, wantSet)
				}
			}
		})
	}
}

func TestExtract_WellDoesNotFalsePositiveEasily(t *testing.T) {
	// Documented limitation: \bwell\b matches "well" even inside idioms like
	// "well-maintained" or "as well" because hyphen/space are word
	// boundaries too. This test locks in the CURRENT, accepted behavior
	// (matches) rather than pretending it's excluded, so a future change to
	// the regex is a deliberate decision, not an accidental regression.
	e := New()
	s := baseSanitized()
	s.Body = "Well-maintained road access, priced to sell"
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if got := it.Attributes["well"]; got != "true" {
		t.Fatalf("Attributes[well] = %q, want %q (documented false-positive behavior)", got, "true")
	}
}

func TestExtract_Location(t *testing.T) {
	e := New()
	s := baseSanitized()
	s.Aspects = map[string]string{"location": "Bend, OR"}
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if got := it.Attributes["location"]; got != "Bend, OR" {
		t.Fatalf("Attributes[location] = %q, want %q", got, "Bend, OR")
	}

	s2 := baseSanitized()
	s2.Aspects = map[string]string{}
	it2, err := e.Extract(context.Background(), s2)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if got, ok := it2.Attributes["location"]; ok {
		t.Fatalf("Attributes[location] present = %q, want absent", got)
	}
}

func TestExtract_APN(t *testing.T) {
	e := New()
	s := baseSanitized()
	s.Body = "Legal description on file. APN: 123-456-789. Buyer to verify."
	it, err := e.Extract(context.Background(), s)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if it.CanonicalID != "123-456-789" {
		t.Fatalf("CanonicalID = %q, want %q", it.CanonicalID, "123-456-789")
	}

	s2 := baseSanitized()
	s2.Body = "No parcel number given in this listing."
	it2, err := e.Extract(context.Background(), s2)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if it2.CanonicalID != "" {
		t.Fatalf("CanonicalID = %q, want empty", it2.CanonicalID)
	}
}

func TestDeterministicID(t *testing.T) {
	e := New()
	s1 := baseSanitized()
	s1.SourceID = "craigslist"
	s1.SourceKey = "abc123"

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
	s2.SourceID = "craigslist"
	s2.SourceKey = "xyz789"
	it2, err := e.Extract(context.Background(), s2)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if it1a.ID == it2.ID {
		t.Fatalf("different SourceKey produced the same id: %q", it1a.ID)
	}
}

func TestTokenize(t *testing.T) {
	got := tokenize("5 Acres 5 Acres in Rural-County, Great Views!")
	want := []string{"acres", "in", "rural", "county", "great", "views"}
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
	got := tokenize("a 5 acres b lot")
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
	if it.Category != "land" {
		t.Fatalf("Category = %q, want %q", it.Category, "land")
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

// baseSanitized returns a minimal, well-formed listing.Sanitized suitable as
// a starting point for test cases that only need to vary one field.
func baseSanitized() listing.Sanitized {
	return listing.Sanitized{
		SourceID:     "craigslist",
		SourceKey:    "7654321",
		SourceURL:    "https://example.invalid/land/7654321.html",
		Title:        "5 Acre Rural Parcel, Great Views",
		Body:         "Beautiful vacant land, buildable, road access.",
		PriceCents:   4500000,
		Currency:     "USD",
		ConditionRaw: "",
		Aspects:      map[string]string{},
		SeenAt:       time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		Boundary:     "test-sanitizer",
	}
}
