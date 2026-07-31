package offer_test

import (
	"testing"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/offer/offerstoretest"
)

// MemoryStore is the REFERENCE implementation of the contract: it runs the same
// suite every persistent adapter must pass, so the two cannot drift.
func TestMemoryStoreSatisfiesTheContract(t *testing.T) {
	offerstoretest.Run(t, func(t *testing.T) offer.Store { return offer.NewMemoryStore() })
}
