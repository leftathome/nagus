# Changelog

All notable changes to nagus are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Wine category on a $0/mo data stack** (see
  `docs/design/2026-08-30-wine-category.md` for the full decision log,
  including the rejection of the paid Wine-Searcher API and its non-perpetual
  "free trial"). The third fill of the category bundle abstraction, needing
  only two generic-spine additions (the `EqAttr` and `HasToken` filter
  predicates below) -- the abstraction held for a category as unlike land and
  HDD as wine, including its international shipping constraints:
  - `internal/identity/lwin` -- entity resolution to the Creative-Commons
    LWIN identifier: CSV load of the Liv-ex export, accent-fold/alias
    normalization, token-blocked token-set-ratio scoring with Jaro-Winkler
    tiebreak, and confidence routing (auto >= 92 / adjudicate >= 80 /
    review). Only auto-route matches ever stamp `CanonicalID` (LWIN-11): a
    wrong canonical identity corrupts every downstream quality join, which
    is worse than none.
  - `internal/valuation/wine` -- critic-score normalization (100-point
    passthrough with plausibility bounds; 20-point Jancis-Robinson-style
    scores through piecewise-linear anchors, deliberately not linear 5x) and
    aggregation with per-critic dedupe and the GWS minimum-3 rule (value is
    never flagged on fewer than 3 independent scores unless the operator
    lowers the bar). Value comes from a hedonic log-price model with a
    quadratic score term plus a 90-point superstar indicator -- the premium
    is non-linear (JWE superstar study; Ali/Lecocq/Visser 2008), so a naive
    points-per-dollar ratio would misprice both tails. Verdicts are
    residual-z tiers; default coefficients are documented cold-start priors.
  - `internal/extract/wine` -- deterministic extractor: vintage, bottle size
    (750ml default; prices are scaled to 750ml-equivalent before valuation),
    varietal/colour dictionaries, and critic attributions ("WS 92",
    "Wine Spectator 92", "JR 17.5") parsed into typed scores. The "WA"
    shorthand for The Wine Advocate is deliberately not recognized -- in
    this home market it is Washington state next to a number.
  - **`internal/shipping` -- ship-legality as a data-driven CONSTRAINT
    LAYER over ~110 JURISDICTIONS worldwide**, not a hardcoded rule for any
    one market. Direct-to-consumer wine law is per-destination and
    per-channel, and the destination is config, not a home market: the
    household buys for itself and buys gifts for people in Barcelona,
    Toronto, or Melbourne. A jurisdiction is an ISO 3166 code -- a country
    (`FR`, `AU`) optionally with a subdivision (`US-WA`, `CA-ON`) -- and a
    source declares its channel (`producer` | `retailer`) plus its origin
    jurisdiction; both required, a missing or malformed declaration is a
    startup error. Each destination carries a policy saying, per channel,
    which ORIGIN RELATIONS may ship to it: same subdivision (in-state), same
    country (interstate), same trade bloc (the EU single market's excise
    distance-selling regime), or foreign. That expresses what actually
    differs -- a WA retailer ships within Washington while a California one
    may not ship in (SB 5007 died in committee, Jan 2024); a French winery
    distance-sells to a Spanish consumer but not a US one, whose imports must
    clear a licensed importer; a BC winery reaches Manitoba but not Ontario.
    `DefaultRules` covers the US per state, Canada per province, the EU-27
    with the single market as a bloc, and other major wine markets (GB, CH,
    AU, NZ, AR, BR, CL, MX, UY, ZA, JP) at country level, with per-region
    confidence documented in `defaults.go`; it is an engineering baseline,
    NOT legal advice, and every destination and bloc is overridable from a
    JSON file merged over it.
    Everything FAILS CLOSED -- unknown/malformed jurisdiction, unmodeled
    destination, unknown channel, unstamped item, empty legal set. Two
    deliberate consequences: the table OMITS destinations whose regime we
    could not state (rather than encoding an all-false entry that would look
    modeled), so `Rules.Modeled` can tell "unmodeled" from "prohibited" and a
    watch configured for an unmodeled destination fails at startup naming the
    override path instead of going silently dark; and the `foreign` dimension
    is off almost everywhere, since markets with personal-import allowances
    (AU, NZ, GB, JP) are a one-line override rather than a default.
    The channel tagger stamps each listing's whole legal-destination SET
    (`ship_legal_to`, tokens validated as ISO 3166 at extract), so one
    ingested corpus serves watches for any destination and a rules change
    converges on the next poll's re-stamp; a surface's `wineShipTo` filters
    to it.
  - **Currency handling, because an international corpus needs it.** The
    hedonic model now declares the currency its coefficients were fit in
    (USD by default) and the valuer takes operator-configured FX rates
    (`wineFxRates`). A listing in an unrated foreign currency is reported
    `unknown-no-reference` -- unplaceable, never mispriced -- because
    comparing a EUR price against a USD-fit model would emit a confident
    wrong verdict, the failure-looks-like-success shape this repo keeps
    re-learning. An EMPTY currency reads as the model's own: a connector
    omitting the field is a data gap, not evidence of a foreign price.
    Rates are config rather than a live FX call, so the read path cannot
    hang on a third party.
- **`score.Filter.EqAttr` and `score.Filter.HasToken`** -- two generic
  attribute predicates (checked between the price bounds and MinAttr,
  deterministic reason order, missing attribute fails with a reason naming
  it). EqAttr requires exact equality; HasToken reads the attribute as a
  space-separated token SET and requires membership, with an empty set
  containing nothing -- so set-valued gates fail closed. The wine bundle
  uses HasToken to gate on the stamped legal-destination set; both stay
  category-agnostic data like every other Filter field.
- **Inquiries: watches gain a duration and a principal** (nagus-7yq). A watch is
  the spec's *Inquiry* -- a standing want held by a principal -- and it now
  carries the two things it was missing: `expires_at`, so a want does not search
  forever, and `principal`, who asked. Principal is deliberately separate from
  `audience`: audience is a delivery routing tag openclaw resolves, principal is
  the requester. They usually coincide, which is why they needed separating
  before anything depends on the difference.
  An expired inquiry is **skipped entirely** rather than returning an empty
  result -- "no longer looking" is not the same as "looked and found nothing" --
  and a lapsed inquiry naming a category with no surface no longer breaks the
  whole evaluation pass. Zero expiry means no expiry, so every existing watch
  keeps working unchanged.
  `Config.ActiveCategories` reports which categories an unexpired inquiry
  references, which is the spec's dormant-vs-active distinction. It currently
  REPORTS activation rather than enforcing it; making it load-bearing is
  deliberately a separate step so it cannot darken a live surface by surprise.
- **Offer-only sources** -- a source may declare no category, in which case it
  feeds the offer store and nothing evaluates it: no glovebox crossing, no
  extraction, no typed item. First increment of gate-at-eval (nagus-7yq), and
  what lets a source be collected SPECULATIVELY -- accumulating history for goods
  no category evaluates yet, so activating one later does not start cold --
  without inventing a category bundle first. Expressed as "no extractor" rather
  than a flag, because that is the actual condition. Offer housekeeping still
  runs, since expiry and retention are properties of the SOURCE. Configuring one
  with the offer layer disabled is a startup error, not a silent no-op.

### Fixed

- **Rate-limited pages are retried, and expiry now requires complete coverage.**
  Two coupled changes, because the first without the second would have made
  things worse.
  Storefronts send `Retry-After` on a 429 (serverpartdeals sends 60), so a
  retry waits exactly as long as the server asked -- obeying a rate limiter
  rather than defeating one. Bounded attempts, a wait cap so a mistaken header
  cannot wedge a source's ingest goroutine, context-aware so shutdown is not
  blocked, and non-rate-limit errors are not retried since that only delays the
  same answer.
  The important half: **expiry is skipped after an incomplete fetch.** Marking an
  offer expired asserts "the source no longer lists this", and that is only sound
  if we saw the whole catalogue. After a truncated or rate-limited walk the
  unseen tail is indistinguishable from a withdrawn listing, so expiring would
  mark LIVE, PURCHASABLE offers as gone -- and a wrongly-expired offer vanishes
  from every recommendation. This was already latent before the retry work, and
  raising the page caps widened the exposure.

- **Images are now built multi-arch (`linux/amd64` + `linux/arm64`)**
  (nagus-viw). orac is a mixed-architecture cluster and an amd64-only nagus
  crashlooped with `exec format error` after a rollout scheduled it onto an
  arm64 node -- taking the service down, because the chart uses
  `strategy: Recreate`. Scheduling had been working by luck: only 2 of the 7
  nodes are amd64.
  CI adopts `homelab/ci-templates` v0.2.0 and its three-job recipe: two NATIVE
  per-arch kaniko legs (no QEMU -- emulation would need a privileged binfmt
  DaemonSet the cluster does not have) publishing immutable `:<sha>-<arch>`
  tags, then a crane merge publishing the deployed tags as an index over those.
  The Dockerfile pins **both** platform digests and selects one per leg, rather
  than switching to the multi-arch index digest: the in-cluster zot mirror
  on-demand-syncs whatever is requested and the full `golang:1.26` index is
  ~3GB across 9 platforms, which the residential uplink cannot pull reliably --
  pinning the index would have re-broken exactly what nagus-c4p fixed. Each leg
  still pulls only its own ~312MB.

- **Chart: a config-only change now rolls the Deployment** (chart 0.5.1). nagus
  reads `NAGUS_CONFIG` and the watches file once at startup, so editing values
  updated the ConfigMap while the running pod kept serving the old config -- and
  `kubectl rollout status` reported success, because the previous rollout really
  was complete. The failure mode was misleading rather than merely inconvenient:
  it bit twice and each time produced a confidently wrong reading (a removed
  source appeared to have no effect; enabling product-identity hints appeared to
  yield zero keyed offers). Both changes were fine; the pod was simply still on
  the old config. Fixed with `checksum/config`, `checksum/watches` and
  `checksum/demo` pod-template annotations over the RENDERED templates, so the
  checksum also moves when a template's own rendering logic changes.

## [0.4.0] - 2026-07-31

### Added

- **Offer layer** (`internal/offer`), opt-in via `offers.enabled` in the chart.
  Offers accumulate from every source regardless of whether any category
  currently evaluates them, so activating a category later does not start cold
  and price history survives for goods nothing scores. Additive: the item store,
  surface, `search_items` and watches are unchanged.
  Expiry and retention are deliberately separate axes -- an offer the source
  stops showing becomes *expired* and is RETAINED as evidence, while per-source
  retention policy is the only thing that deletes. An expired offer must never
  reach a purchase recommendation, so `offer.Query` returns only purchasable
  offers unless `IncludeExpired` is set.
- **Three store adapters** (memory / sqlite / postgres) all passing one shared
  reference contract, so offers and items are peer stores. On postgres the offer
  tables live in the SAME database as items; on sqlite they are a separate file.
- **Per-source retention** replaces a per-category hardcode that applied eBay's
  6h content window to every hdd source -- including storefronts with no such
  obligation, which a few hours of rate-limiting would have wiped.
- **Product hints from Shopify sources**, declared per store rather than guessed,
  making cross-seller dedup real: 250 offers in the serverpartdeals catalogue
  resolve to 132 distinct products.

## [0.3.1] - 2026-07-31

### Fixed

- **HDD $/TB reference is now derived from our own ingested offers** instead of
  fetching a third-party catalog on every search
  (`internal/valuation/hdd.StoreSource`). With eBay live, every filter survivor
  was scoring `unknown-no-reference`: the old reference was one retailer's
  `products.json?limit=250`, whose capacities are enterprise 16-24TB drives plus
  a lot of SSDs, so the entire 6-14TB band where most listings actually live had
  no anchor at all. It was also a partial catalog (one page, no pagination) and
  put a live third-party call -- against a host that rate limits hard -- on the
  read path of a read-only surface.
  Measured against the live corpus: 23 of 30 (capacity, condition) buckets now
  carry enough comparables to anchor a reference, the densest being 10TB refurb
  with 17 -- precisely the band that previously had none.
  **This changes what the reference MEANS**: from "what one retailer charges" to
  "the median of comparable offers nagus has actually seen". That is a market
  reference; it is better for spotting a deal but moves with the market. The live
  `ShopifySource` remains available for injection, it is simply no longer the
  default.
  Guarded against the self-comparison trap: a reference computed over the same
  corpus being scored would, for a listing with no comparables, return that
  listing's own price -- a ratio of exactly 1.0, scoring "market" forever while
  looking authoritative. Below `MinSamples` (default 3) it reports no reference
  instead, because unknown is honest and self-referential is not.

## [0.3.0] - 2026-07-30

### Added

- **Shopify `products.json` connector** (`internal/connector/shopify`): one generic
  connector over the public, unauthenticated storefront feed, configured per
  retailer (`type: shopify`, with `baseUrl`, `productTypePrefixes`,
  `includeUnavailable`, `maxPages`). No credentials and no cost. This gives the
  **hdd** category a working live source, which it has lacked entirely since eBay
  Browse went dark behind the ungranted production keyset.
  Encodes three things the published Shopify schema does not state, found by
  capturing real catalogs: capacity is often NOT in the product title (it lives in
  `product_type` and a `capacity:` tag); condition is carried in tags, with new and
  refurbished listed as separate products; and `product_type` prefixes are
  inconsistent for one category, so the allow-filter takes a list. Also handles
  per-variant capacities, payloads that are not valid UTF-8, and aggressive
  storefront rate limiting (429 as a distinct error -- poll hourly at most).
- **Typed capacity path in the hdd extractor**: a structured connector may supply
  `Aspects["capacity_tb"]` and the extractor prefers it over scanning the title,
  falling back to the title when it is absent, non-numeric or nonpositive. Without
  this, sources that title products by model number would have every item dropped
  by the capacity hard-filter.

### Fixed

- **Land enrichment now resolves at the parcel, not the city centroid.** Flood and
  wetland signals were derived by geocoding `Attributes["location"]`, which for the
  Zillapi source is a city label -- so those signals described downtown rather than
  the parcel, and fed the verdict that decides whether to notify. Exact
  per-listing coordinates are now used directly (geocoding remains the fallback for
  place-name-only sources) and are validated first, so null-island, unparseable and
  out-of-range values fall back instead of enriching confidently at the wrong
  place. The parcel provider is now given a street address rather than a city name,
  which it cannot resolve. Root cause was the land extractor silently discarding
  the `lat`/`lon` aspects the connector had been emitting all along.

## [0.2.0] - 2026-07-30

### Compliance

- **The Craigslist source was a Terms of Use violation and is removed.**
  Craigslist's ToU prohibits copying or collecting their content "via robots,
  spiders, scripts, scrapers, crawlers, or any automated or manual equivalent" --
  a blanket prohibition on automated collection. Consuming a search feed they
  publish is still automated collection, so the connector was in violation from
  the start, not merely once the feed was withdrawn. **Craigslist is not a
  supported source for nagus and must not be reintroduced** in any form: not the
  RSS feed, not the internal JSON search API, not a headless browser, and not by
  relocating the same fetch into another service -- moving code does not change
  consent. It was removed as soon as this was recognized.

### Added

- **Zillapi land connector** (`internal/connector/zillapi`): the land acquisition
  source replacing the retired Craigslist feed, built against Zillapi's OpenAPI
  3.1 contract (`POST /v1/search`, bearer auth). Wired as source `type: zillapi`
  with a per-source bounding box, `maxItems` and `daysOnZillow`; the key syncs
  from Vault `eso/nagus/sources` -> `zillapi_key` -> `NAGUS_ZILLAPI_KEY` via
  `land.zillapiExternalSecret`.
  **Zillapi bills one credit per RESULT returned, not per call**, so the acreage
  and price window from the category config is pushed UPSTREAM as a spend control,
  the result cap is explicit, and a land source is meant to poll DAILY. See
  `docs/design/2026-07-29-zillapi-land-connector.md`.
- **Typed acreage path in the land extractor**: a structured connector may supply
  `Aspects["acreage"]` (already in acres) and the extractor prefers it over
  scanning free text, instead of an API connector having to compose prose for a
  regex to re-parse. A junk or nonpositive aspect falls back to the text scan.

### Removed

- **Craigslist connector** (`internal/connector/craigslist`) and all its wiring
  (the `craigslist` source type, the `-craigslist-*` ingest flags, the
  `NAGUS_CL_*` env vars, and `land.craigslistCity` / `land.craigslistCategory`
  in the chart). Craigslist retired the `?format=rss` search feed the connector
  read: every URL carrying `format=rss` now returns their block page (HTTP 403),
  while the plain HTML search still returns 200 -- so this was an endpoint
  retirement, not an IP or User-Agent block. Craigslist's Terms of Use prohibit
  automated collection and circumventing access controls, so there is no
  compliant replacement fetch path and the connector is deleted rather than
  ported.
- **Consequence:** land acquisition now runs through the Zillapi source above
  instead, and is reachable only via the multi-source config path -- that
  connector is anchored on a bounding box, which the legacy single-source flags
  cannot express. `nagus ingest -category land` fails with an error pointing at
  the config path instead of silently collecting nothing, and the legacy
  `serve -category land` path resolves to zero sources (surface-only). Land
  scoring, extraction, geo enrichment, and Rentcast enrichment are unchanged.

## [0.1.0] - 2026-07-03

First stable release: the generic acquisition/watch spine with two reference
category bundles (HDD and land), two storage backends, a read-only surface
(HTTP + MCP), delivery watches, and a Helm chart. It finds and reports; it never
acts (eyes, not hands).

### Added

- **Spine** (`internal/pipeline`): generic, category-agnostic
  connector -> sanitize -> extract -> normalize -> store -> hard-filter -> enrich
  -> score -> surface. The hard-filter runs before enrichment (bounds paid-API
  volume to survivors).
- **Item model + contracts** (`internal/item`, `internal/listing`): the
  normalized item and the `Raw -> Sanitizer -> Sanitized -> Extractor -> Item`
  chain, with the glovebox trust boundary modeled as a gate (positional trust,
  byte-preserved content).
- **Stores** (`internal/store`): a swappable `Store` interface with two adapters
  that pass the same `MemoryStore` reference contract -- **SQLite+FTS5**
  (`sqlitestore`, pure-Go, default) and **PostgreSQL** (`postgresstore`,
  pgx/pgxpool, shared CloudNativePG cluster; FTS-only, pgvector deferred).
- **HDD category**: eBay Browse connector (`internal/connector/ebay`, OAuth +
  fixture mode), deterministic capacity/condition extractor
  (`internal/extract/hdd`), and category-reference `$/TB` valuation
  (`internal/valuation/hdd`) with a great/good/market/poor verdict.
- **Land category**: nagus-direct Craigslist RSS connector
  (`internal/connector/craigslist`), land extractor (acreage, well/septic/fixer
  flags, APN; `internal/extract/land`), free US-gov geo enrichment
  (`internal/enrich/geo`: FEMA flood, USGS elevation, USDA soil, USFWS wetlands,
  Census geocode) and a swappable parcel adapter (`internal/enrich/parcel`,
  Rentcast default), scored **structure-first** (structure + land-value-dominant
  + low flood + price fit -> great; flood AE/VE or wetlands downgrade).
- **Scoring** (`internal/score`): deterministic hard-filter + verdict-to-score
  ranking over a category-generic deal signal.
- **Surface** (`nagus serve`): a read-only process exposing an **MCP server**
  at `/mcp` (JSON-RPC 2.0; tools `search_items`, `get_item`), plain-HTTP
  `/search` + `/item`, and `/watches`, plus an optional in-process ingest loop.
- **Delivery watches** (`internal/watch`): a watch = saved `search_items` query
  + notify threshold; `/watches` returns per-watch candidates (quiet inbox) and
  strong matches (ping) with an opaque audience tag for openclaw's resolver.
- **Helm chart** (`charts/nagus`): sqlite (PVC) or postgres backend, demo mode,
  watches ConfigMap, land config, Vault-backed ExternalSecrets. Single-writer
  pod (one replica, Recreate).
- **CI/release**: GitLab primary (kaniko image + OCI chart push + release via
  `homelab/ci-templates`) and a GitHub mirror publishing the image + chart to
  `ghcr.io/leftathome` on tag.

### Security

- All listing content is untrusted and crosses the glovebox boundary before any
  LLM instruction context; the extract stage emits a constrained typed schema
  (bad data, never hijack).
- `search_items` and every HTTP/MCP surface are read-only -- no mutating tool is
  exposed. Non-GET requests are rejected.
- No secrets in git: eBay/Rentcast/Postgres credentials come from Vault via
  external-secrets.

### Known limitations

- External data sources are validated against fixtures only; live-key validation
  is tracked (eBay gated keyset, Rentcast, gov geo endpoints, Shopify `$/TB`).
- Query-time enrichment (land geo/parcel signals) is not yet persisted into the
  surfaced rows -- only the verdict survives.
- Postgres text search is substring (`ILIKE`) to match the reference contract;
  ranked FTS/pgvector is a follow-on.

[0.4.0]: https://gitlab.orac.local/agentic/nagus/-/releases/v0.4.0
[0.3.1]: https://gitlab.orac.local/agentic/nagus/-/releases/v0.3.1
[0.3.0]: https://gitlab.orac.local/agentic/nagus/-/releases/v0.3.0
[0.2.0]: https://gitlab.orac.local/agentic/nagus/-/releases/v0.2.0
[0.1.0]: https://gitlab.orac.local/agentic/nagus/-/releases/v0.1.0
