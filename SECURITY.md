# Security policy

## Reporting

Report suspected vulnerabilities privately to the maintainer rather than opening a
public issue. Include reproduction steps and affected version/commit.

## Threat model notes

nagus ingests untrusted, potentially adversarial third-party listing content.
Core invariants:

- All external listing content crosses the glovebox sanitization boundary before
  any processing.
- The extract/tokenize stage emits a constrained typed schema; free text is
  carried as data and quoted, never as agent instructions (prompt-injection
  containment).
- The `search_items` interface is read-only. nagus performs no purchases, bids, or
  seller contact; such actions are out of scope and human-gated elsewhere.
- Third-party API credentials are provided at runtime from Vault and never
  committed to the repository.

## eBay user data: no-PII posture (Marketplace Account Deletion opt-out)

nagus stores **no eBay user personal data**, which is why it qualifies for the
opt-out from eBay's Marketplace Account Deletion/Closure notification rather than
implementing the deletion endpoint. The guarantees behind that attestation:

- **No seller identifier is stored.** Nothing keyed to a seller (username, id, or
  any hash/HMAC of one) is ever persisted or logged. A hash of an enumerable
  username is pseudonymous, not anonymous, and a per-seller keyed record would
  itself be the profile the policy protects -- so we don't create one. The
  username is decoded only as a **transient argument** to the optional per-seller
  profile lookup (account-age / recent-sales tiers), held at most in an ephemeral
  per-fetch cache that is discarded when the fetch returns, and never written to
  an aspect, item, or log.
- **Snapshot-only.** The profile lookup takes a fresh point-in-time reading each
  fetch; nagus never accumulates per-seller history across runs (that would
  rebuild a keyed profile). The feature is OFF by default.
- **Only coarse, non-identifying buckets are stored, on the item.** Seller-quality
  signals are derived from eBay's PUBLIC data into tiers written to the *listing's*
  `item.Attributes` (`seller_feedback_tier`, `seller_volume_tier`, `ships_from_us`),
  never keyed to the seller. Raw values (exact feedback %, exact counts) are never
  persisted -- a fine-grained tuple would be a quasi-identifier.
- **Snapshot-only.** Every seller signal is derived from a single point-in-time API
  read. nagus never accumulates per-seller observations across runs; doing so would
  rebuild a keyed profile and void the opt-out.
- When a seller deletes their eBay account, eBay removes the listing (the stored
  `SourceURL` 404s) and the coarse tiers point at no live identity -- there is
  nothing about a specific user to erase, by construction.

Enforcement: `internal/extract/hdd` (bucketing) and `internal/connector/ebay`
(no-username decode) carry tests, including `TestExtract_NeverStoresSellerIdentity`
and `TestFetch_SellerAspects_NoUsername`, that fail if a username or raw seller
value could reach a persisted field.
