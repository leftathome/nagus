package promo

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/deal"
	"github.com/leftathome/nagus/internal/offer"
)

// A stored product is re-judged on restore: a product today's rules refuse
// (or one judged under another rules version) makes the message be
// processed again instead of being re-emitted.
func TestRestoreRejudgesStoredProducts(t *testing.T) {
	line := func(category, title, url string) string {
		b, _ := json.Marshal(map[string]any{"schema": deal.SchemaID, "category": category, "title": title, "price": "10.00", "url": url, "currency": "USD", "seller": "Newegg"})
		return string(b)
	}
	for name, sp := range map[string]storedProduct{
		"laptop now refused":    {Rules: RulesVersion, Line: line("hdd", "MSI Summit E13 Flip Evo 13.4in Laptop 1TB", "https://www.newegg.com/p/N82E16834156677")},
		"other rules version":   {Rules: "1999-01-01", Line: line("hdd", "WD Red Plus 12TB NAS Hard Drive", "https://www.newegg.com/p/N82E16822234441")},
		"no rules version":      {Line: line("hdd", "WD Red Plus 12TB NAS Hard Drive", "https://www.newegg.com/p/N82E16822234441")},
		"other category stored": {Rules: RulesVersion, Line: line("wine", "Tolaini Al Passo Toscana 2022", "https://www.wine.com/product/x/1")},
	} {
		t.Run(name, func(t *testing.T) {
			state := offer.NewMemoryStore()
			r := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD, perPoll: 1000, perDay: 1000, state: state}, "newegg.html")
			b, _ := json.Marshal([]storedProduct{sp})
			_ = state.PutPromoMessage(context.Background(), offer.PromoMessage{
				SourceID: r.src, MessageKey: MessageKey(neweggSender, neweggMsg(t).ID),
				ProcessedAt: testNow, Outcome: MsgProcessed, RulesVersion: RulesVersion, Products: b,
			})
			r.parse(t, neweggMsg(t))
			if r.m.Messages(r.src, MsgRestored) != 0 || r.fv.calls.Load() == 0 {
				t.Fatal("a product today's rules refuse was re-emitted")
			}
		})
	}
	// A wine.com product whose URL slug no longer matches its title.
	state := offer.NewMemoryStore()
	r := newPRig(t, rigOpts{profile: "winecom", category: deal.CategoryWine, perPoll: 1000, perDay: 1000, state: state}, "winecom.html")
	b, _ := json.Marshal([]storedProduct{{Rules: RulesVersion, Line: line("wine", "Tolaini Al Passo Toscana 2022", "https://www.wine.com/product/caymus-cabernet-sauvignon-2021/1")}})
	msgID := "wc-rejudge@example.com"
	_ = state.PutPromoMessage(context.Background(), offer.PromoMessage{SourceID: r.src, MessageKey: MessageKey(wineSender, msgID), ProcessedAt: testNow, RulesVersion: RulesVersion, Products: b})
	if pm, _, _ := state.GetPromoMessage(context.Background(), r.src, MessageKey(wineSender, msgID)); len(pm.Products) == 0 {
		t.Fatal("setup")
	}
	e := &msgEntry{}
	if r.p.restore(e, MessageKey(wineSender, msgID)) {
		t.Fatal("a product whose slug no longer matches was restored")
	}
}

// slowState blocks every usage write until released.
type slowState struct {
	*offer.MemoryStore
	release chan struct{}
	once    sync.Once
	entered chan struct{}
}

func (s *slowState) AddVisionUsage(ctx context.Context, day string, delta int) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.MemoryStore.AddVisionUsage(ctx, day, delta)
}

// A stalled usage write never blocks another source's reservation: the
// database is written outside the budget's lock.
func TestBudgetWritesOutsideTheLock(t *testing.T) {
	st := &slowState{MemoryStore: offer.NewMemoryStore(), release: make(chan struct{}), entered: make(chan struct{})}
	b := NewBudget(100, func() time.Time { return testNow })
	b.State = st
	go func() { _, _ = b.NewPoll(10).Reserve(2) }() // stalls in the write
	<-st.entered
	done := make(chan bool, 1)
	go func() {
		ok, _ := b.NewPoll(10).Available(2)
		done <- ok
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("budget refused")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled database write blocked another source's budget check")
	}
	if b.Stats().UsedToday != 2 {
		t.Fatalf("memory count %d, want 2 (memory is authoritative)", b.Stats().UsedToday)
	}
	close(st.release)
}
