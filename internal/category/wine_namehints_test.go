package category

import (
	"testing"

	"github.com/leftathome/nagus/internal/store"
)

// The switch that used to let a wine source stamp LWIN ids now opts it in to
// sending quark name hints (quark QUARK-04), keyed on the producer aspect the
// channel tagger stamps. Off, the source's offers carry whatever structured
// hint it states, as before.
func TestWineIngesterNameHintsFollowTheStampSwitch(t *testing.T) {
	src := mustSource(t, "producer", "US-WA")
	on, err := NewWineIngester(&fakeWineConn{id: "p"}, src, WineDeps{Store: store.NewMemoryStore(), LWINStamp: true, Producer: "Leonetti Cellar"})
	if err != nil {
		t.Fatal(err)
	}
	if on.NameHintProducer != "wine_producer" {
		t.Fatalf("opted-in source: NameHintProducer = %q, want wine_producer", on.NameHintProducer)
	}
	off, err := NewWineIngester(&fakeWineConn{id: "p"}, src, WineDeps{Store: store.NewMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	if off.NameHintProducer != "" {
		t.Fatalf("source not opted in sends name hints: %q", off.NameHintProducer)
	}
}
