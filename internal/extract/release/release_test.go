package release

import (
	"context"
	"testing"

	"github.com/leftathome/nagus/internal/listing"
)

func TestExtractLiftsKnownAspectsOnly(t *testing.T) {
	it, err := New().Extract(context.Background(), listing.Sanitized{
		SourceID: "ttbcola:a", SourceKey: "26012001000650", Title: "LEONETTI CELLAR",
		Aspects: map[string]string{"brand": "LEONETTI CELLAR", "permit": "BW-WA-67", "approval_date": "2026-01-15", "junk": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if it.Category != "release" || it.PriceCents != 0 || it.Attributes["permit"] != "BW-WA-67" || it.Attributes["junk"] != "" {
		t.Fatalf("item %+v", it)
	}
	if len(it.Tokens) == 0 || it.Tokens[0] != "leonetti" {
		t.Fatalf("tokens %v", it.Tokens)
	}
	if _, err := New().Extract(context.Background(), listing.Sanitized{SourceID: "ttbcola:a"}); err == nil {
		t.Fatal("no source key must be an error")
	}
}
