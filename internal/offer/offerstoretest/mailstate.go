package offerstoretest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
)

// runMailState is the MailState contract (nagus-p7x); Run calls it for every
// adapter, which must all implement offer.MailState.
func runMailState(t *testing.T, newStore NewStore) {
	t.Run("MailStateImplemented", func(t *testing.T) {
		if _, ok := newStore(t).(offer.MailState); !ok {
			t.Fatal("the store does not implement offer.MailState")
		}
	})
	ms := func(t *testing.T) offer.MailState {
		s, ok := newStore(t).(offer.MailState)
		if !ok {
			t.Skip("no MailState")
		}
		return s
	}
	t.Run("PromoMessageRoundTrips", func(t *testing.T) { promoMessageRoundTrips(t, ms(t)) })
	t.Run("PromoReadingRoundTrips", func(t *testing.T) { promoReadingRoundTrips(t, ms(t)) })
	t.Run("VisionUsageAccumulates", func(t *testing.T) { visionUsageAccumulates(t, ms(t)) })
	t.Run("PurgeMailState", func(t *testing.T) { purgeMailState(t, ms(t)) })
	t.Run("AuthPassRoundTrips", func(t *testing.T) { authPassRoundTrips(t, ms(t)) })
	t.Run("PurgeAuthPasses", func(t *testing.T) { purgeAuthPasses(t, ms(t)) })
}

// authPassRoundTrips: a pass is stored per (source, message key), replaced
// in place, and needs its identity and path (nagus-aoq).
func authPassRoundTrips(t *testing.T, s offer.MailState) {
	ctx := context.Background()
	if _, ok, err := s.GetAuthPass(ctx, "imap:deals", "k1"); err != nil || ok {
		t.Fatalf("missing pass: ok %v err %v", ok, err)
	}
	if err := s.PutAuthPass(ctx, "imap:deals", "k1", "dkim", T0); err != nil {
		t.Fatal(err)
	}
	if p, ok, err := s.GetAuthPass(ctx, "imap:deals", "k1"); err != nil || !ok || p != "dkim" {
		t.Fatalf("round trip: %q ok %v err %v", p, ok, err)
	}
	if err := s.PutAuthPass(ctx, "imap:deals", "k1", "arc", T1); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.GetAuthPass(ctx, "imap:deals", "k1"); p != "arc" {
		t.Fatalf("replace: %q", p)
	}
	if _, ok, _ := s.GetAuthPass(ctx, "imap:other", "k1"); ok {
		t.Fatal("a pass leaked across sources")
	}
	for _, bad := range [][3]string{{"", "k", "dkim"}, {"imap:deals", "", "dkim"}, {"imap:deals", "k", ""}} {
		if err := s.PutAuthPass(ctx, bad[0], bad[1], bad[2], T0); !errors.Is(err, offer.ErrNoAuthPassKey) {
			t.Fatalf("put %q: %v, want ErrNoAuthPassKey", bad, err)
		}
	}
}

// purgeAuthPasses deletes only the named source's passes recorded before
// the cutoff, and leaves the shared promo state alone.
func purgeAuthPasses(t *testing.T, s offer.MailState) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutAuthPass(ctx, "imap:deals", "old", "dkim", T0))
	must(s.PutAuthPass(ctx, "imap:deals", "new", "dkim", T1))
	must(s.PutAuthPass(ctx, "imap:other", "old", "dkim", T0))
	must(s.PutPromoReading(ctx, "img", []byte(`[]`), T0))
	must(s.PurgeAuthPasses(ctx, "imap:deals", T1))
	if _, ok, _ := s.GetAuthPass(ctx, "imap:deals", "old"); ok {
		t.Fatal("a pass before the cutoff survived")
	}
	if _, ok, _ := s.GetAuthPass(ctx, "imap:deals", "new"); !ok {
		t.Fatal("a pass at the cutoff was purged")
	}
	if _, ok, _ := s.GetAuthPass(ctx, "imap:other", "old"); !ok {
		t.Fatal("another source's pass was purged")
	}
	if _, ok, _ := s.GetPromoReading(ctx, "img"); !ok {
		t.Fatal("purging passes purged shared promo state")
	}
}

func promoMessageRoundTrips(t *testing.T, s offer.MailState) {
	ctx := context.Background()
	if _, ok, err := s.GetPromoMessage(ctx, "imap:promo", "k1"); err != nil || ok {
		t.Fatalf("missing message: ok %v err %v", ok, err)
	}
	pm := offer.PromoMessage{
		SourceID: "imap:promo", MessageKey: "k1", UIDValidity: 4000000001, UID: 4000000002,
		ProcessedAt: T0, Outcome: "processed", RulesVersion: "r1", Products: []byte(`[{"line":"{}"}]`),
	}
	if err := s.PutPromoMessage(ctx, pm); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetPromoMessage(ctx, "imap:promo", "k1")
	if err != nil || !ok || got.UIDValidity != pm.UIDValidity || got.UID != pm.UID || !got.ProcessedAt.Equal(T0) ||
		got.Outcome != "processed" || got.RulesVersion != "r1" || string(got.Products) != string(pm.Products) {
		t.Fatalf("round trip: %+v ok %v err %v", got, ok, err)
	}
	// Replace, and scoped by source.
	pm.Outcome, pm.Products = "processed", []byte(`[]`)
	if err := s.PutPromoMessage(ctx, pm); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetPromoMessage(ctx, "imap:promo", "k1"); string(got.Products) != `[]` {
		t.Fatalf("replace: %q", got.Products)
	}
	if _, ok, _ := s.GetPromoMessage(ctx, "imap:other", "k1"); ok {
		t.Fatal("a message leaked across sources")
	}
	if err := s.PutPromoMessage(ctx, offer.PromoMessage{SourceID: "imap:promo"}); !errors.Is(err, offer.ErrNoMessageKey) {
		t.Fatalf("no key: %v", err)
	}
}

func promoReadingRoundTrips(t *testing.T, s offer.MailState) {
	ctx := context.Background()
	if _, ok, err := s.GetPromoReading(ctx, "abc"); err != nil || ok {
		t.Fatalf("missing reading: %v %v", ok, err)
	}
	if err := s.PutPromoReading(ctx, "abc", []byte(`[{"products":[]},{"products":[]}]`), T0); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPromoReading(ctx, "abc", []byte(`[1,2]`), T1); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetPromoReading(ctx, "abc")
	if err != nil || !ok || string(got) != `[1,2]` {
		t.Fatalf("reading %q %v %v", got, ok, err)
	}
	if err := s.PutPromoReading(ctx, "", []byte("x"), T0); err == nil {
		t.Fatal("a reading without a hash was stored")
	}
}

func visionUsageAccumulates(t *testing.T, s offer.MailState) {
	ctx := context.Background()
	day := offer.UsageDay(T0)
	if n, err := s.VisionUsage(ctx, day); err != nil || n != 0 {
		t.Fatalf("empty day: %d %v", n, err)
	}
	for _, d := range []int{2, 2, 2, -1} {
		if err := s.AddVisionUsage(ctx, day, d); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.VisionUsage(ctx, day); n != 5 {
		t.Fatalf("usage %d, want 5", n)
	}
	// Never below zero, and days are separate.
	if err := s.AddVisionUsage(ctx, day, -100); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.VisionUsage(ctx, day); n != 0 {
		t.Fatalf("usage %d after an over-refund, want 0", n)
	}
	if err := s.AddVisionUsage(ctx, "2099-01-01", 3); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.VisionUsage(ctx, day); n != 0 {
		t.Fatal("days are not separate")
	}
}

func purgeMailState(t *testing.T, s offer.MailState) {
	ctx := context.Background()
	old, recent := T0, T0.Add(48*time.Hour)
	for _, pm := range []offer.PromoMessage{
		{SourceID: "imap:a", MessageKey: "old", ProcessedAt: old},
		{SourceID: "imap:a", MessageKey: "new", ProcessedAt: recent},
		{SourceID: "imap:b", MessageKey: "old", ProcessedAt: old},
	} {
		if err := s.PutPromoMessage(ctx, pm); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.PutPromoReading(ctx, "old", []byte("x"), old)
	_ = s.PutPromoReading(ctx, "new", []byte("y"), recent)
	_ = s.AddVisionUsage(ctx, offer.UsageDay(old), 4)
	_ = s.AddVisionUsage(ctx, offer.UsageDay(recent), 6)
	if err := s.PurgeMailState(ctx, "imap:a", T0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetPromoMessage(ctx, "imap:a", "old"); ok {
		t.Error("an old message of the source survived")
	}
	if _, ok, _ := s.GetPromoMessage(ctx, "imap:a", "new"); !ok {
		t.Error("a recent message was purged")
	}
	if _, ok, _ := s.GetPromoMessage(ctx, "imap:b", "old"); !ok {
		t.Error("another source's message was purged")
	}
	if _, ok, _ := s.GetPromoReading(ctx, "old"); ok {
		t.Error("an old reading survived")
	}
	if _, ok, _ := s.GetPromoReading(ctx, "new"); !ok {
		t.Error("a recent reading was purged")
	}
	if n, _ := s.VisionUsage(ctx, offer.UsageDay(old)); n != 0 {
		t.Error("an old day's usage survived")
	}
	if n, _ := s.VisionUsage(ctx, offer.UsageDay(recent)); n != 6 {
		t.Error("a recent day's usage was purged")
	}
}
