package ebay

import (
	"context"
	"strconv"
)

// SellerProfile holds eBay-public seller signals that are NOT in the Browse
// item_summary response and require a second, per-seller API call (account age,
// a recent-sales proxy). Only COARSE buckets derived from these ever persist;
// the raw values and the seller username never do (data minimization -- the
// eBay account-deletion opt-out posture; see SECURITY.md).
type SellerProfile struct {
	AccountAgeDays int // days since the seller's eBay registration date
	RecentSales    int // recent feedback count, a point-in-time proxy for recent sales
	// HasAccountAge / HasRecentSales report which fields the source resolved.
	// Unresolved fields are omitted (no aspect) rather than emitted as a
	// misleading 0.
	HasAccountAge  bool
	HasRecentSales bool
}

// SellerProfileSource resolves the second-call seller signals for a username.
//
// token is a valid eBay OAuth access token the implementation may present as the
// API's auth (the eBay Shopping API GetUserProfile requires the token in the
// X-EBAY-API-IAF-TOKEN header -- verified live against the sandbox 2026-07). The
// username is a lookup argument ONLY: implementations MUST treat it as transient
// and MUST NOT persist or log it. found=false means "no profile available" (the
// caller simply omits the tiers). Sourcing a fresh snapshot each call is required
// -- nagus never accumulates per-seller history across runs, which would rebuild
// a keyed profile and void the opt-out.
type SellerProfileSource interface {
	Profile(ctx context.Context, username, token string) (p SellerProfile, found bool, err error)
}

// sellerProfileResult is one resolved (or resolved-absent) profile, cached per
// fetch to dedupe repeat sellers without re-spending the API budget.
type sellerProfileResult struct {
	prof  SellerProfile
	found bool
}

// enrichSellerProfile adds the second-call seller signals to aspects, using
// username ONLY as a transient lookup key (never written to aspects). Results
// are cached in the caller's per-fetch map so repeat sellers cost one call. A
// spent budget or a transient error simply skips enrichment -- the listing still
// stands. Raw values are emitted as aspects for the extractor to BUCKET; the
// raw day/count never persist on the item.
func (c *Connector) enrichSellerProfile(ctx context.Context, username, token string, aspects map[string]string, cache map[string]sellerProfileResult) {
	res, seen := cache[username]
	if !seen {
		// Account for the profile call against the daily budget; on exhaustion,
		// degrade (do not cache, do not circumvent).
		if err := c.budget.reserve(); err != nil {
			return
		}
		prof, found, err := c.cfg.SellerProfile.Profile(ctx, username, token)
		res = sellerProfileResult{prof: prof, found: found && err == nil}
		// Cache success or error-as-absent to avoid re-calling for the same seller
		// within this fetch.
		cache[username] = res
	}
	if !res.found {
		return
	}
	if res.prof.HasAccountAge {
		aspects["seller_account_age_days"] = strconv.Itoa(res.prof.AccountAgeDays)
	}
	if res.prof.HasRecentSales {
		aspects["seller_recent_sales"] = strconv.Itoa(res.prof.RecentSales)
	}
}
