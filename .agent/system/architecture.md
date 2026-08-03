# nagus architecture

Last verified against a running deployment: 2026-08-02.

Authoritative design lives in `docs/design/2026-07-01-nagus-design.md` (READ IT
before changing architecture) and `docs/superpowers/specs/2026-07-26-nagus-offer-product-rearchitecture.md`.
This file is the orientation you want *before* those: what exists, why it is
shaped this way, and which parts are load-bearing.

## What nagus is

Monitor sources → sanitize → extract → normalize → store → hard-filter → enrich
→ score → surface. It finds and reports; **it never acts.** `search_items` is
eyes, not hands: no auto-buy, no bidding, no contacting sellers.

## The two stores, and why there are two

    SOURCES ──► OFFER STORE ──────────────► (evaluation) ──► ITEM STORE ──► SURFACE
               every listing, always        only for            typed,        search_items
               untrusted bytes at rest      active categories   scored        /search /watches

**Item store** (`internal/store`) holds typed, category-specific items. It feeds
the live read surface. Adapters: memory / sqlite / postgres, all passing one
reference contract.

**Offer store** (`internal/offer`) is a layer IN FRONT. It records every listing
a source returns, whether or not any category evaluates it. This exists so that
activating a category later does not cold-start, and so price history survives
for goods nothing currently scores. Same three adapters, same contract discipline
(`internal/offer/offerstoretest` is the specification; each adapter is one
implementation).

On **postgres** both live in ONE database as separate, loosely-related table
sets — backup and restore are then a single unit, so item and offer state cannot
be restored out of step. On **sqlite** the offer store is a separate FILE,
because SQLite's constraints are real there: a VACUUM takes a database-wide lock
and the pool is capped at one connection, so sharing a file would put offer
maintenance in front of the read path.

## Three distinctions that are easy to conflate and expensive to get wrong

1. **Expiry is not deletion.** An offer the source stops showing becomes
   `expired` and is RETAINED — "vendor X ran this at $Y last week" is real
   evidence. Only per-source *retention policy* deletes.
2. **Expired is not purchasable.** An expired listing is gone; recommending it
   wastes the reader's time at best. `offer.Query` therefore returns only
   purchasable offers unless `IncludeExpired` is set. Surfacing history is always
   deliberate.
3. **A listing is not a transaction.** Anyone can list anything at any price, so
   an offer's existence is weak price evidence; made-AND-fulfilled is strong.
   `Offer.Outcome` (unknown|sold|unsold) carries that, defaulting to unknown.

## Retention is a property of the SOURCE

Not of the category evaluating it. eBay's License 8.1(b) requires forgetting
eBay Content within ~6h; a Shopify storefront has no such obligation, and purging
it would have been actively harmful (a few hours of rate-limiting would have
wiped its corpus). `cmd/nagus.retentionForSource` is the policy table.
`summarize-decay` is carried in config but deliberately REJECTED at validation
rather than silently downgraded — both plausible downgrades are wrong in ways
nobody would notice.

## Sources

Connectors live in `internal/connector/`. A connector returns `listing.Raw`;
`Title`/`Body`/aspect VALUES are UNTRUSTED and are never interpreted.

| Connector | Notes |
|---|---|
| `ebay` | Browse API, OAuth client_credentials. Budgeted (~5k calls/day). |
| `shopify` | ONE generic connector, N store configs. Public `/products.json`, no auth. |
| `zillapi` | Land. Built and tested, **not deployed** — bills per RESULT returned. |

**Craigslist is a PROHIBITED source** — see the hard rule in CLAUDE.md.

A source may declare **no category**, making it offer-only: it feeds the offer
store with no glovebox crossing, no extraction, no items. That is gate-at-eval's
first increment and how speculative sources (savemyserver) are collected.

### Per-store configuration is declared, not inferred

Product-identity hints are the sharp example. One store's `vendor` is the real
manufacturer ("Western Digital"); another's is its own house label on generic
hardware. Inferring brand from `vendor` would mint false product identities, so
`brandTag` / `skuIsMpn` / `skuSuffixes` are per-store and default to emitting
nothing. A store whose data cannot identify a product stays ungrouped — the
honest answer.

## Scoring and the reference

The `$/TB` reference derives from **our own ingested corpus**
(`valuation/hdd.StoreSource`), not a live third-party fetch. The old approach
fetched one retailer's catalogue page per query, which left the entire 6-14TB
band unscoreable and put an external call on the read path. `MinSamples` (default
3) guards the self-comparison trap: a reference computed over the corpus being
scored would otherwise return a listing's own price, scoring "market" forever
while looking authoritative.

## Deployment

Flux HelmRelease in `steve/gitops` → `clusters/orac/apps/nagus/`. Chart in
`charts/nagus`. Images are multi-arch (the cluster is mostly arm64; an amd64-only
image once crashlooped a rollout into an outage). Secrets come from Vault via
ExternalSecrets — never in `config.json`, which is committed via a ConfigMap.

## Where the bodies are buried

- **Image tags are 8-char SHAs**, not 7.
- **`kubectl rollout status` can report success for a change that did not
  apply** — it describes the *previous* rollout. Chart 0.5.1 added checksum
  annotations so config-only edits roll the pod; before that they silently did
  not.
- **Paginated fetches can cap silently.** The connector now warns; two live
  sources had been truncated for days.
- **Expiry requires complete coverage** — a partial fetch cannot distinguish a
  withdrawn listing from one it never reached.
- **The CI amd64 leg OOMs intermittently** under node memory pressure. Retrying
  the job usually succeeds.
- **Zillapi bills per RESULT**, so the poll window must equal the poll interval
  or every listing is re-billed each cycle.
