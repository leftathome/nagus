// Package offer is the offer layer that sits IN FRONT of the typed item store.
//
// An offer is a specific listing at a specific source selling some good. Many
// offers refer to one product (N:1) -- eBay, serverpartdeals and waterpanther
// each have their own offer for the same drive. Product identity belongs to the
// manufacturer, not the seller, so it is NOT resolved here; see ProvisionalKey.
//
// # Why this layer exists
//
// Today a listing is extracted into a typed, category-specific item at ingest
// and only that is stored, so a source producing goods we do not currently
// evaluate is simply dropped -- and starting to evaluate them later means a cold
// start with no history. Offers accumulate cheaply regardless of whether any
// category currently evaluates them.
//
// # Offers at rest are UNTRUSTED
//
// Title and Body hold source free text. This package stores and returns those
// bytes; it never interprets them, and nothing downstream may treat them as
// instructions. The glovebox crossing and category extraction fire at the point
// of USE, not here (gate-at-evaluation).
//
// # Expiry is not deletion, and expired is not purchasable
//
// These are deliberately separate axes, because conflating them loses real
// signal or produces dangerous recommendations:
//
//   - LIFECYCLE: an offer that stops appearing at its source becomes Expired. It
//     is still valuable evidence -- "vendor X offered this product at a good
//     price last week as a daily deal" says something about vendor X's future
//     pricing, and about what this product's price floor really is. So expiry
//     RETAINS the row.
//   - RETENTION: whether an offer's row may be kept at all, and for how long, is
//     a per-source policy (see Retention) driven by that source's terms. It is
//     the only thing that deletes.
//
// The safety-critical consequence: **an expired offer must never reach a
// purchase recommendation.** It can be evaluated for deal quality and it can
// inform a judgement about a vendor, but it cannot be something we point a human
// at to buy -- the listing is gone. That is enforced structurally rather than by
// convention: Query returns only purchasable offers unless IncludeExpired is set,
// so surfacing history is always a deliberate act.
package offer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status is where an offer sits in its lifecycle.
type Status string

const (
	// StatusActive means the offer was present at its source as of the last
	// poll: it is purchasable.
	StatusActive Status = "active"
	// StatusExpired means the offer no longer appears at its source. It is
	// retained as historical signal and MUST NOT be recommended for purchase.
	StatusExpired Status = "expired"
)

// Purchasable reports whether this offer may be put in front of a human as
// something to buy. Only active offers qualify: an expired offer's listing is
// gone, so recommending it wastes the reader's time at best and misleads at
// worst. Historical analysis should read Status directly rather than call this.
func (o Offer) Purchasable() bool { return o.Status == StatusActive }

// Outcome records how an offer ENDED, which is a different question from
// whether it is currently purchasable.
//
// This distinction carries real evidential weight. Anyone can list anything at
// any price, so the mere existence of an offer at price X is weak evidence that
// X is a real market price -- especially on an open marketplace. An offer that
// was made AND FULFILLED is much stronger evidence. Downstream weighting is not
// this package's job, but the model must carry the distinction or downstream
// cannot weigh it, and "we saw a listing" would silently masquerade as "we saw a
// sale".
type Outcome string

const (
	// OutcomeUnknown is the default: we never observed how the offer ended.
	// Treat it as WEAK evidence of price -- it may simply never have sold.
	OutcomeUnknown Outcome = ""
	// OutcomeSold means the offer was observed fulfilled. Strong evidence.
	OutcomeSold Outcome = "sold"
	// OutcomeUnsold means the offer was observed ending without a sale, which is
	// itself informative: the asking price did not clear.
	OutcomeUnsold Outcome = "unsold"
)

// ProductHint carries the raw product identifiers a source exposes, verbatim.
// These are UNTRUSTED source strings; they are matching material, not truth.
// quark will own authoritative product identity later.
type ProductHint struct {
	Brand string
	MPN   string
	GTIN  string
	Model string
}

// Offer is one listing at one source.
type Offer struct {
	// ID is deterministic from SourceID+SourceKey so re-ingesting the same
	// listing updates its lifecycle rather than creating a duplicate.
	ID string
	// SourceID is the connector identity, e.g. "shopify:serverpartdeals".
	SourceID string
	// SourceKey is the source-native id.
	SourceKey string
	// SourceURL is the canonical link back to the listing.
	SourceURL string

	// Title and Body are UNTRUSTED source free text stored verbatim.
	Title string
	Body  string

	// Trusted scalars, derived by the connector.
	PriceCents int64
	Currency   string
	Condition  string
	// Seller identifies the vendor where the source exposes one. This is what
	// makes "vendor X tends to discount product A" answerable later, so it is
	// retained on expired offers too.
	Seller string

	// Aspects are source structured attributes: keys trusted, VALUES UNTRUSTED.
	Aspects map[string]string

	// ProvisionalKey groups offers believed to be the same product across
	// sellers. It is a deliberately imperfect, throwaway stand-in for quark's
	// authoritative productID -- see ComputeProvisionalKey.
	ProvisionalKey string
	ProductHint    ProductHint

	// --- lifecycle ---------------------------------------------------------
	// FirstSeen is when this offer was first observed.
	FirstSeen time.Time
	// LastSeen is the most recent observation. Expiry is derived from this.
	LastSeen time.Time
	// MinPriceSeen is the lowest price ever observed for this offer, which is
	// what makes discount detection possible after the discount has ended.
	MinPriceSeen int64
	// Status is the lifecycle state. See Purchasable.
	Status Status
	// Outcome records how the offer ended, when observed. See Outcome: an
	// unfulfilled listing is much weaker price evidence than a completed sale.
	Outcome Outcome
	// ExpiredAt is when the offer was marked expired; zero while active.
	ExpiredAt time.Time
}

// Errors returned by this package.
var (
	ErrNoSourceID  = errors.New("offer: SourceID is required")
	ErrNoSourceKey = errors.New("offer: SourceKey is required")
	ErrNegPrice    = errors.New("offer: PriceCents must not be negative")
)

// Validate reports whether the offer can be persisted. Price 0 is legal and
// means "unknown/unpriced", matching the item contract; negative is not.
func (o Offer) Validate() error {
	if strings.TrimSpace(o.SourceID) == "" {
		return ErrNoSourceID
	}
	if strings.TrimSpace(o.SourceKey) == "" {
		return ErrNoSourceKey
	}
	if o.PriceCents < 0 {
		return fmt.Errorf("%w: %d", ErrNegPrice, o.PriceCents)
	}
	return nil
}

// DeterministicID derives a stable offer id from source identity, so the same
// listing seen again updates in place.
func DeterministicID(sourceID, sourceKey string) string {
	sum := sha256.Sum256([]byte(sourceID + "\x00" + sourceKey))
	return hex.EncodeToString(sum[:])[:16]
}

// ComputeProvisionalKey derives a best-effort cross-seller grouping key from the
// identifiers a source happens to expose: MPN if present, else brand+model.
//
// It is EXPECTED to be imperfect against real, messy, cross-source data. It is
// not entity resolution and must not be treated as authoritative -- when quark
// ships it supersedes this with a real productID and the matching logic here is
// thrown away. Returns "" when there is nothing to key on, which callers must
// treat as "ungrouped", never as a group of its own.
func ComputeProvisionalKey(h ProductHint) string {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	if mpn := norm(h.MPN); mpn != "" {
		return "mpn:" + mpn
	}
	brand, model := norm(h.Brand), norm(h.Model)
	if brand != "" && model != "" {
		return "bm:" + brand + ":" + model
	}
	return ""
}

// --- retention ----------------------------------------------------------------

// RetentionPolicy is how long a SOURCE's offers may be kept. Retention is a
// property of the source (its terms), not of the category evaluating it.
type RetentionPolicy string

const (
	// RetainFull keeps offers and their lifecycle indefinitely.
	RetainFull RetentionPolicy = "retain-full"
	// Purge hard-deletes offers past the window. The conservative fallback.
	Purge RetentionPolicy = "purge"
	// SummarizeDecay drops live listing detail at the window but keeps a coarse
	// historical data point ("this product WAS offered here for X on <date>"),
	// with resolution coarsening as it recedes.
	//
	// NOT YET IMPLEMENTED as a transformation -- see Retention.Validate. The
	// architecture carries the policy per source because whether a given source
	// MAY use it is a compliance judgement, not an architectural one.
	//
	// Two constraints the eventual summary schema must satisfy, both per-source:
	// it may need to record only that AN OFFER WAS MADE ON THE PLATFORM without
	// tying a seller to it (Retention.RetainSeller), and the summary should
	// carry Outcome, because "was listed at X" and "sold at X" are very
	// different evidence and a summary that conflates them is worse than no
	// summary.
	SummarizeDecay RetentionPolicy = "summarize-decay"
)

// Retention is one source's retention configuration.
type Retention struct {
	Policy RetentionPolicy
	// Window is how long offers are kept before Policy applies. Ignored by
	// RetainFull.
	Window time.Duration

	// RetainSeller permits keeping the Seller identity in whatever survives the
	// window. It defaults to FALSE, which is the safe reading: some sources'
	// terms allow recording that an offer was made on the platform but NOT
	// tying seller information to a particular offer.
	//
	// This is why it lives on the SOURCE's policy rather than being a global
	// choice -- it is a per-source terms question. It has no effect until
	// SummarizeDecay is implemented (Purge deletes the row entirely and
	// RetainFull is only for sources whose terms permit full retention), but it
	// is declared here so that the summary schema cannot be written without
	// confronting it.
	RetainSeller bool
}

// ErrUnsupportedPolicy is returned for a policy this build cannot honour.
var ErrUnsupportedPolicy = errors.New("offer: unsupported retention policy")

// Validate rejects a retention config this build cannot honour.
//
// SummarizeDecay is deliberately rejected rather than silently downgraded to
// Purge or RetainFull: either silent choice would be wrong in a way nobody
// would notice -- one destroys signal the operator asked to keep, the other
// keeps data the source's terms may forbid.
func (r Retention) Validate() error {
	switch r.Policy {
	case RetainFull:
		return nil
	case Purge:
		if r.Window <= 0 {
			return fmt.Errorf("offer: purge retention needs a positive window")
		}
		return nil
	case SummarizeDecay:
		return fmt.Errorf("%w: %q is carried in config but not yet implemented; a source may only use it once its summary schema is validated against that source's terms", ErrUnsupportedPolicy, r.Policy)
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedPolicy, r.Policy)
	}
}

// --- store --------------------------------------------------------------------

// Query selects offers.
//
// The zero value returns only PURCHASABLE offers. Including expired ones is
// opt-in precisely because the dangerous mistake -- putting a dead listing in
// front of someone as a thing to buy -- is the one a default should not make.
type Query struct {
	// SourceID limits to one source; "" = any.
	SourceID string
	// ProvisionalKey limits to one provisional product group; "" = any.
	ProvisionalKey string
	// Seller limits to one vendor; "" = any.
	Seller string
	// Since limits to offers last seen at or after this time; zero = no bound.
	Since time.Time
	// IncludeExpired additionally returns offers that are NO LONGER
	// PURCHASABLE. Set it for price history, vendor behaviour, or deal-quality
	// analysis -- never for anything that recommends a purchase.
	IncludeExpired bool
	// Limit caps rows; 0 = no limit.
	Limit int
}

// Store persists offers. Implementations MUST be safe for concurrent use.
//
// MemoryStore is the reference contract: a new adapter should pass the same
// tests, exactly as the item store's adapters do.
type Store interface {
	// Put inserts or updates an offer by ID, folding lifecycle: FirstSeen is
	// preserved from any existing row, LastSeen advances, MinPriceSeen tracks
	// the lowest price ever seen, and re-appearance revives an expired offer.
	Put(ctx context.Context, o Offer) error
	// Get returns one offer by id.
	Get(ctx context.Context, id string) (Offer, bool, error)
	// Query returns offers matching q.
	Query(ctx context.Context, q Query) ([]Offer, error)
	// MarkExpired transitions every offer of a source not seen since the cutoff
	// to StatusExpired, returning how many changed. This is HOUSEKEEPING, not
	// deletion: expired offers are retained as signal. It must run regardless of
	// whether any category currently evaluates the source.
	MarkExpired(ctx context.Context, sourceID string, notSeenSince time.Time, now time.Time) (int, error)
	// ApplyRetention enforces a source's retention policy, returning how many
	// offers it removed. This is the ONLY operation that deletes.
	ApplyRetention(ctx context.Context, sourceID string, r Retention, now time.Time) (int, error)
}
