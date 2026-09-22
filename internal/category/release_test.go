package category

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/connector/ttbcola"
	"github.com/leftathome/nagus/internal/store"
)

// End to end over the real registry export: approvals ingest unpriced, and the
// verdict separates a fresh label (pings) from an old one (quiet).
func TestReleaseIngestAndVerdictByAge(t *testing.T) {
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) // 17 days after Leonetti's 2026-01-15 approval
	st := store.NewMemoryStore()
	conn := ttbcola.NewConnector(ttbcola.Config{Name: "allocation", FixturePath: "../connector/ttbcola/testdata/quilceda.csv", Now: func() time.Time { return now }})
	res, err := NewReleaseIngester(conn, ReleaseDeps{Store: st}).Ingest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 4 {
		t.Fatalf("stored %d, want 4 unpriced approvals", res.Stored)
	}
	out, err := NewReleaseSurface(ReleaseDeps{Store: st, Now: func() time.Time { return now }}).Surface(context.Background(), store.Query{Category: "release", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]string{}
	for _, sc := range out.Items {
		verdicts[sc.Item.Title] = sc.Signal.Verdict
		if sc.Item.PriceCents != 0 || sc.Item.Attributes["permit"] == "" {
			t.Fatalf("item %+v", sc.Item)
		}
	}
	if len(verdicts) != 4 {
		t.Fatalf("surfaced %d, want all 4 (no price requirement): %v", len(verdicts), verdicts)
	}
	if verdicts["LEONETTI CELLAR"] != VerdictNewLabel || verdicts["QUILCEDA CREEK PALENGAT"] != VerdictOldLabel {
		t.Fatalf("verdicts %v", verdicts)
	}
}
