# Design spec: multi-category acquisition / watch subsystem

- **Status:** draft (brainstorm converged 2026-06 through 2026-07-01)
- **Bead:** openclaw-5e8s
- **Codename:** **Quartermaster** (placeholder — a quartermaster procures both provisions and equipment, which matches the two product classes; rename freely)
- **Repo:** a NEW standalone GitLab project, not openclaw and not glovebox (see Section 12)
- **Supersedes/absorbs:** openclaw-qb9w (land-search cron) becomes one category instance, not a bespoke cron

## 1. Problem statement

The operator (and household) repeatedly wants to know when a *good* instance of some
item appears — a 5-acre parcel with a structure under $X near Y; a refurb 16 TB
enterprise HDD under a $/TB threshold; a retiring Lego set below market; groceries on
the shopping list dropping locally. Today this is manual, ad hoc, and the one attempt
to do it in an interactive agent session (openclaw-qb9w) exposed the session-contamination
and tool-reliability problems that motivated the eval/heartbeat work.

The generalizable need is a **watch pipeline**: monitor sources -> sanitize untrusted
listing content -> normalize -> hard-filter -> enrich with category-specific signals ->
score -> surface. It is *not* a conversational "help me shop" skill.

### Prior art (why we build)

Researched 2026-06/07 (see Appendix A). Summary:

- **Point shopping skills exist in abundance** on ClawHub (`shop`, `price`, `zillow`,
  `ebay`, `amazon`, `price-tracker-pro`, `clawmart-price-monitor`, ...) but they are
  **conversational, single-source, in-agent** helpers. The two headline ones we
  inspected (`zillow`, `price`) are **pure advisory prompt skills with no data source,
  no persistence, no scheduler**. The real monitors that exist target Chinese-market
  e-commerce and run in-agent, not on connectors.
- **Nobody has built the connector-based watch pipeline** (monitor -> filter -> enrich ->
  score -> surface) with a normalized store and a query endpoint. The real-estate
  geo-enrichment (flood/elevation/soil/drive-time) is green-field.
- **OSS price trackers** (Discount-Bandit, PriceBuddy, changedetection.io) solve *pieces*
  and are worth reusing/adapting (Section 11). `jayyala/openclawshopping` is a
  non-functional AI-generated demo — skip.
- **Security lesson — "ClawHavoc"** (Nov 2025-Feb 2026): a typosquatting supply-chain
  campaign planted ~1,184+ malicious ClawHub skills (AMOS infostealer, `SOUL.md`/`MEMORY.md`
  persistence); ClawHub's own scanner mislabeled ~91% of confirmed threats as benign.
  **Consequence for us: anything touching money/credentials stays out-of-process behind
  the glovebox boundary; we never install community shopping skills into an agent's
  context.**

## 2. Goals and non-goals

**Goals**
- Watch many sources across many categories with one generic pipeline.
- Treat all listing content as untrusted (prompt-injection boundary via glovebox).
- Enrich with category-specific signals that define "good" (not just price).
- Surface, don't decide: **eyes not hands** — the system finds and reports; a human (or a
  separately-gated path) acts.
- Homelab scale, single operator + household; fast iteration; cheap by default.

**Non-goals (v1)**
- Auto-purchase, auto-bid, auto-contact-seller. Any *action* is a separate, human-gated
  path, never a capability of the watcher or its search endpoint.
- Legally authoritative determinations ("this parcel is buildable", "this seller is a
  scammer"). We surface *signals that change the calculus*, not verdicts.
- Beating dedicated sneaker/land bots at adversarial, anti-bot-hardened sources.

## 3. Two product classes

The operator's examples split cleanly into two products that **share a spine** but differ
in cadence, matching, enrichment, and delivery. They are not variants of one thing.

| | **Class A — recurring / hyper-local consumables** | **Class B — rare "like to have" durables/collectibles** |
|---|---|---|
| Examples | groceries, local variable goods | land, cars, servers, HDD/DRAM/CPU/GPU, UniFi, Lego, Nike |
| Cadence | continuous / weekly-ad-driven | event-driven (new listing appears) |
| Matching | standing list x current local prices | criteria/wishlist x incoming listing -> deal-quality |
| Geography | tight (drivable stores) | broad (durables) or specific (land) |
| Counterparty facet | none (retailers) | central (seller trust / scam risk) |
| Enrichment | unit-price, historical-low | category valuation + condition + geo (land) |
| Delivery | periodic digest + strong-drop ping | quiet inbox of candidates + strong-match ping |

## 4. Architecture: the spine

```
source
  -> connector (per source; mostly shared: eBay, RSS, thin API, scrape-backend)
  -> glovebox: SANITIZE untrusted content (prompt-injection boundary)
  -> EXTRACT / TOKENIZE (sanitized free-text -> typed fields + FTS tokens)
  -> NORMALIZE (canonical item schema; price-on-the-source multi-listing model)
  -> STORE (structured store + memory-core bridge)
  -> HARD-FILTER (deterministic; cheap; runs before any paid enrichment)
  -> ENRICH (pluggable facets: item / counterparty / risk)
  -> SCORE (deterministic gate, then LLM ranks the short list)
  -> SURFACE:
       push: quiet inbox + strong-match ping   (watches = saved queries + threshold)
       pull: search_items MCP tool              (read-only, eyes-not-hands)
```

Key ordering invariant: **hard-filter before enrich.** Enrichment hits paid APIs
(Regrid/ATTOM parcels, KicksDB, etc.); running it only on survivors of the cheap
deterministic filter bounds cost and latency. This is a first-class design constraint,
not an optimization.

## 5. The core abstraction: category = a plugin bundle

A "category" is config + adapters over the generic spine. Adding a category is a bundle,
not a new system.

```
category = {
  connectors:      [ebay, craigslist-rss, niche...],   # mostly shared
  canonical_id:    <extractor: set# | part# | style-code+size | VIN | APN | ...>,
  valuation:       <adapter: category-reference-api | scrape-tool | ebay-solds | built>,
  condition_model: new/refurb/used | size-curve | structure-present | none,
  match_mode:      deal-vs-market | restock-at-MSRP | appreciation-timing | basket-price,
  seller_trust:    structured(ebay) | heuristic(craigslist) | none(retailer),
  enrichment:      [item-facet, counterparty-facet, risk-facet],   # facets, per class
  delivery:        quiet-inbox + strong-ping | periodic-digest,
}
```

Land, groceries, and every hardware/collectible category are different fills of this
struct. The diverse operator examples **validated** the abstraction rather than stretching
it.

### Match modes (the matching stage is itself pluggable)
- **deal-vs-market** (most durables): listing price vs category valuation, condition/size-adjusted.
- **restock-at-MSRP** (UniFi, hyped GPUs, SNKRS): primary-retailer availability, not valuation. Bridges toward Class A availability-watching.
- **appreciation-timing** (retired Lego): temporal signal ("retiring soon / just retired") -> buy before the premium.
- **basket-price** (Class A groceries): standing list vs current local prices.

## 6. Component: connectors

- **eBay Browse API is the common denominator for Class B** — cars, electronics, servers,
  HDD, DRAM, CPU, GPU, UniFi-secondary, Lego, Nike all list on eBay, with structured
  condition (New / Manufacturer-refurbished / Seller-refurbished / Used), price, and
  aspects (size, style code, set #). **Sold/completed comps require the Marketplace
  Insights API (gated/restricted access)** — see Section 9 valuation decision. One solid
  eBay connector covers listing acquisition for most of Class B: N categories = 1 eBay
  connector + N valuation adapters, not N integrations.
- **RSS reuses the EXISTING glovebox RSS connector** (`glovebox.io/connector=rss`,
  already running ~10 feeds on a 30m poll). Craigslist for-sale/real-estate-by-owner is
  RSS: `https://<city>.craigslist.org/search/<cat>?format=rss` (cat `sss` all-for-sale,
  `reo` real-estate-by-owner, `cta` cars). **Adding Craigslist = one feed entry**, not a
  new component. Polite cadence (30-60m) to avoid IP blocks; ~25 items/feed, no bodies.
- **Thin API connectors** for sources with real/affordable APIs (Zillapi land $5/mo,
  BrickLink free, Kroger, Best Buy, etc.).
- **Scrape-only sources (v2): changedetection.io as a watch backend behind glovebox** —
  for LandWatch/Land.com/county-sale/JS-only pages that have neither feed nor API. It is
  Apache-2.0, has a full REST API (create watch, `?recheck=1`, `/history`, `/difference`),
  restock/price mode (`restock_diff` parses schema.org JSON-LD), and Apprise output
  (`json://` -> a glovebox receiver connector). **Browser mode connects to our existing
  Browserless (v2.38.1) via its CDP endpoint** (`connect_over_cdp`, NOT the version-locked
  `/playwright` server endpoint) — no second browser container. Caveats: Browserless
  session-timeout cap; add the pod to Browserless's `allow-consumers` NetworkPolicy;
  browser mode is per-watch opt-in (RSS/HTML/JSON fetch in plain-HTTP mode needs no browser).

## 7. Component: sanitize + extract/tokenize

After glovebox sanitizes raw listing prose, an extraction stage converts prose -> typed
fields + a full-text index:
- **Canonical IDs** (set #, part #, style code, VIN, APN) — regex/pattern + validation.
- **Attributes** (capacity, DIMM rank/speed, shoe size, acreage) — regex/dictionary.
- **Boolean flags** (fixer/teardown/as-is, well+septic, "ships" on a local-pickup category,
  manufacturer-refurb) — keyword classifier.
- **FTS tokens** over the sanitized description for keyword search.

Deterministic-first, LLM-fallback for fuzzy signals. **Injection-containment property:**
the extractor's output is a *constrained typed schema*. Even when a small LLM does the
fuzzy classification, it runs on already-sanitized text and emits typed labels (a
category, a flag, a number), never free execution. Worst case for a malicious listing is a
*wrong field value* (bad data), never agent hijack. The output schema is a second boundary
after input scrubbing.

## 8. Component: normalized item store

- **Item model:** one abstract item, many source-listings; **price lives on the listing,
  not the item** (adapted from Discount-Bandit's Product <-> pivot <-> Link model). One
  item can be tracked across eBay + Craigslist + a niche source.
- **Enables cross-listing correlation** — same phone/image/post across many cities is a
  top scam tell; only computable from a central corpus. This is an argument for a shared
  store over fire-and-forget alerting.
- **Hybrid index (two readers over one corpus):**
  | Index | Good at | Bad at |
  |---|---|---|
  | memory-core (reuse openclaw-3wz extraPaths bridge) | semantic/ambient recall | ranges, sort-by-$/metric, aggregates |
  | structured store (new; SQLite/DuckDB + FTS5) | `$/TB<15 AND cap>=16TB AND refurb AND last 7d, sorted` + comp medians | fuzzy NL recall |
  Bridge extracted markdown into memory-core (near-free, ambient recall) AND write typed
  rows into the structured store fronting the `search_items` tool.
- Store tech is an open decision (Section 15): SQLite+FTS5 vs DuckDB vs Postgres.

## 9. Component: enrichment (facets)

Enrichment is a set of pluggable scorers, not one step. Three facets:

| Class | Item facet | Counterparty facet | Risk facet |
|---|---|---|---|
| Land | flood/elev/soil/slope + **structure/entitlement** | seller mostly N/A | title/lien (future) |
| Cars | VIN history, price-vs-market | eBay rating / dealer / heuristic | odometer/title-brand, scam heuristics |
| Electronics/HW | $/metric, price history, spec match | eBay feedback / heuristic | too-good-to-be-true, stolen-photo |
| Lego/Nike | BrickLink/KicksDB valuation, retirement | eBay feedback | authenticity, per-size sanity |
| Groceries | unit-price, historical-low | none (retailers) | none |

### Valuation decision: category-reference preferred over gated eBay sold-comps
eBay *live* listings are free (Browse); eBay *sold comps* need the gated Marketplace
Insights API. Where a clean category-reference exists, prefer it — usually free/cheaper and
sidesteps the gating:

| Strategy | When | Examples |
|---|---|---|
| category-reference | default where a reference exists | Lego->BrickLink guide (free API, sold+current per set), HDD->diskprices $/TB, CPU->PassMark/$, sneaker->KicksDB per-size |
| eBay sold-comps (Marketplace Insights) | fallback for long-tail items with no reference DB | odd/unique items |

Keep **valuation math on typed API fields** (trusted-ish); keep listing prose out of the
reasoning path except as data-to-be-matched.

### Land enrichment stack (the green-field differentiator)
Survivors of the hard-filter get: **Census geocode -> [FEMA NFHL flood, USGS 3DEP
elevation, USDA SSURGO soil, USFWS NWI wetlands]** — all FREE US-gov REST, no key. Plus
**Regrid** (paid, per-record) for `improvval` + `yearbuilt` + building-footprint + zoning
label. Drive-time is the only gap (OpenRouteService/OSRM). Because enrichment runs only on
filtered survivors, paid parcel-API volume stays tiny.

**Structure-present detection (fixer-upper widening):** `improvval>0 AND yearbuilt`
OR'd with Regrid's matched building-footprint layer — the OR catches manufactured/mobile
homes off the tax roll and new-build lag, which is exactly the rural-fixer case. Scoring:
`structure_present AND land_value_dominant` = high signal (inherited entitlements, short
permit path). **Permit *history*** is partly programmatic (Shovels.ai/county Socrata,
weak rural coverage, address-keyed); **permit *rules*/zoning entitlement is NOT
programmatic** (~30k jurisdictions, ordinance text) -> per-county human step. Fixer/teardown
= keyword classifier over description (RESO `PropertyCondition="Fixer"` only via a real
MLS/IDX feed, which the scrapers don't expose); manufactured = structured field everywhere.

### Seller-trust degrades gracefully per source
- **eBay** = clean structured trust (feedbackScore, feedback %, top-rated flag).
- **Craigslist** = anonymized -> heuristic only (cross-city duplicate post, "ships" on
  local-pickup, price outlier, throwaway-email patterns).
- **Facebook Marketplace** = has exactly the wished-for profile signals but is the most
  closed (login-walled, hostile anti-bot, no API). **"Trust signal unavailable" is a
  first-class state**; the scorer must not assume every source can be scored.
This facet is adversarial and lands on the glovebox boundary by design.

## 10. Component: scoring, and 11. delivery

- **Scoring:** deterministic hard-filter first (price/spec/region — free, bounds enrichment
  cost), then an LLM ranks/annotates the short list of survivors. Match-mode-specific.
- **Delivery push:** a **watch is a saved `search_items` query + a notify threshold.**
  Quiet inbox for candidates; strong-match ping for the rare great one; Class A gets a
  periodic digest. Reuses per-audience/household routing (openclaw-3wz resolver) so items
  reach the right person's agent/inbox.
- **Delivery pull — the `search_items` MCP tool** (Section 11 detail below).

## 11. The search endpoint (pull) and its boundary

```
search_items(category, filters{ price<, price_per_tb<, acreage>=, condition,
             structure_present, seen_since }, sort, limit) -> typed rows
get_item(id) -> full normalized item
```

Push and pull are the **same query interface over one corpus** — watches persist a query;
agents run queries on demand.

**Boundary discipline:**
- Read-only, outbound from the agent's view (agent queries *our* structured store) ->
  squarely in the "structured API = direct access is fine" lane, unlike raw listing prose.
- Results are predominantly typed fields (price, $/metric, flags, IDs) = safe to reason over.
- Any free-text field returned is already glovebox-sanitized **and marked quoted/untrusted**,
  so an embedded "ignore previous instructions" is matched as a string, never executed.
  Sanitize-once-at-ingest + quote-at-query = defense in depth.
- **Eyes not hands:** the endpoint searches and reads. It cannot message a seller, bid, or
  buy. Any action is a separate, human-gated path.

## 12. Reuse vs build (the new GitLab project's scope)

| Build (green-field, this project) | Adapt (design, re-implement) | Reuse as-is |
|---|---|---|
| the spine (pipeline orchestration) | PriceBuddy's data-driven JSON `scrape_strategy` (GPL-3.0 -> design only) | glovebox connectors + sanitization + staging |
| normalized item store + `search_items` MCP | Discount-Bandit's item<->listing model (license `None` -> design only) | existing glovebox **RSS connector** |
| extract/tokenize stage | category valuation adapters | **changedetection.io** (Apache-2.0) as scrape backend (v2) |
| enrichment facets (esp. land geo) | seller-trust heuristics | **Browserless** (v2.38.1, CDP) for JS scrape |
| scoring + match modes | | **memory-core** extraPaths bridge (openclaw-3wz) |
| per-category bundles | | free US-gov geo APIs (FEMA/USGS/USDA/USFWS/Census) |
| | | category valuation APIs (BrickLink free; Zillapi/KicksDB/Regrid paid) |
| | | ClawHub as a *distribution* channel (never in-process) |

### 12.1 Deployment (Helm) and the Postgres/CNPG option

nagus ships a **Helm chart** (`charts/nagus`), patterned on `charts/glovebox`,
published to GHCR by CI on tag. `values.storage.backend` selects the store adapter:

- **`sqlite`** (default): a PVC-backed single-file store. Zero external deps.
- **`postgres`**: the shared **CloudNativePG** cluster `postgres` in namespace
  `databases-app`. The app does NOT bundle Postgres and the chart does NOT create
  the database -- per the homelab pattern, provisioning is a **gitops request** in
  `clusters/orac/foundation/databases-app/`:
  1. add a managed role `nagus` to `cluster-postgres.yaml` (`spec.managed.roles`,
     `passwordSecret: nagus-db-role`),
  2. add `database-nagus.yaml` (`kind: Database`, `cluster.name: postgres`,
     `name: nagus`, `owner: nagus`),
  3. add `externalsecret-nagus-role.yaml` (Vault-backed basic-auth, mirroring
     `externalsecret-aether-role.yaml`).
  The chart then consumes the `nagus-db-role` secret and connects to
  `postgres-rw.databases-app.svc.cluster.local:5432`.

**pgvector caveat:** the shared cluster image `ghcr.io/cloudnative-pg/postgresql:17`
does not include pgvector. Vector/semantic search over the corpus therefore needs
either a pgvector-capable image on the shared cluster (impacts the other tenants:
aether, registry) or a dedicated nagus CNPG `Cluster`. v1 Postgres is FTS-only;
pgvector is deferred. Semantic recall in v1 comes from the memory-core bridge
(section 8), not pgvector.

### Repo boundaries
- **glovebox** (existing repo): connectors + sanitization + the new **extract/tokenize** +
  emitting normalized items. Cross-repo work -> glovebox beads.
- **Quartermaster** (NEW GitLab project): the spine, the normalized item store, the
  enrichment facets, scoring, category bundles, and the `search_items` MCP server. Its own
  OCI image(s), its own Helm/deploy, deployed to the cluster like glovebox/recognizer.
- **openclaw** (this repo): register the MCP tool for agents, per-agent/per-audience access
  scoping (rides the household/resolver privacy model), delivery cron/inbox wiring. openclaw
  beads + config.
- **archiver/recognizer**: unaffected (separate lane).

## 13. Security model (summary)

1. All listing content is untrusted -> glovebox sanitization before anything else.
2. Extraction output is a constrained typed schema -> injection containment (bad data, not
   hijack).
3. Search endpoint is read-only, returns typed fields, quotes any free-text, eyes-not-hands.
4. Community skills (ClawHub) stay out-of-process behind glovebox (ClawHavoc lesson);
   if ever used, `verify --card` + commit-pin, never blind install.
5. Per-audience privacy: items route via the household resolver; agents query only their
   permitted categories.
6. Paid-API keys in Vault, synced to runtime (never committed).

## 14. Cost model

- **Free tier:** Craigslist RSS, BrickLink API, all US-gov geo APIs, memory-core, eBay
  Browse (live listings), diskprices/LabGopher/PassMark scrape, changedetection.io/Browserless
  (already owned).
- **Paid, low-volume by design:** Zillapi (~$5/mo land), Regrid/ATTOM (per-record parcels),
  KicksDB (~EUR29-79/mo sneakers), Keepa (Amazon). Filter-before-enrich keeps call volume
  to filtered survivors.
- **Gated (apply if needed):** eBay Marketplace Insights (sold comps), Zillow Bridge RESO
  (MLS-affiliated only), Shovels.ai ($599/mo permits).

## 15. Open decisions / decision gates

1. **Project name** (Quartermaster placeholder).
2. **v1 reference category(ies).** Recommendation: two reference implementations that
   exercise different axes — **Land** (geo-enrichment + paid-parcel + RSS/Zillopi, the
   green-field) and **one hardware category with a clean $/metric** — strongest pick is
   **HDD $/TB**, which gained a FREE JSON valuation API (PricePerGig/DatacenterDisk; see
   A.8/A.10) so the reference-valuation adapter is trivial — exercising eBay + condition
   new/refurb/used. Lego is the
   cleanest *free* valuation adapter and a good third/showcase (appreciation mode).
3. **Store tech:** DECIDED -- a swappable `store.Store` adapter with two impls:
   **SQLite+FTS5** (default, single-file, homelab) and **Postgres** on the shared
   CloudNativePG cluster (see 12.1). Postgres text search = FTS/tsvector for v1;
   **pgvector is a gated follow-on** (the shared CNPG image lacks the extension).
4. **memory-core bridge vs standalone** for semantic recall (reuse openclaw-3wz vs new).
5. **Parcel provider:** Regrid vs ATTOM vs Rentcast (coverage vs cost vs fields).
6. **changedetection.io in v1 or v2** — v1 land is RSS+Zillapi (no browser); changedetection.io
   enters when a scrape-only source (LandWatch) is added.
7. Whether Quartermaster hosts the extract/tokenize stage or glovebox does.

## 16. v1 cut (proposed, minimal)

Build the **spine + eBay connector + Craigslist(RSS reuse) + the structured store +
`search_items` MCP + two reference adapters (Land, one hardware $/metric)**. No
changedetection.io, no browser, no FB Marketplace, no auto-action. Land exercises
geo-enrichment + the paid-parcel path + fixer/structure detection; the hardware category
exercises eBay + condition axis + category-reference valuation. Adding Lego / DRAM / UniFi /
Nike afterwards is a weekend adapter, not a new system.

---

## Appendix A: source access map (research 2026-06/07)

Status: COMPLETE. All nine source-area tables below (land, parcel/permit, Lego, Nike,
OSS-projects, cars/electronics, groceries, homelab-hardware, seller-trust), plus A.10
corrections to body assumptions surfaced by the research.

### A.1 Land / rural real estate
| Source | Bucket |
|---|---|
| Craigslist reo/land | (a) RSS — `?format=rss`, reuse glovebox RSS connector |
| Zillow land | (c) paid wrapper — Zillapi ~$5/mo (search includes lots); Bridge RESO free but MLS-gated |
| Realtor.com | (c) paid wrapper — RapidAPI |
| Regrid (parcels/enrichment) | (b/c) paid API — improvval/yearbuilt/zoning/footprint |
| LandWatch, Land.com/CoStar, Redfin, county tax-sale | (d/e) scrape-only/hostile — v2 changedetection.io |
| FEMA/USGS/USDA/USFWS/Census enrichment | FREE gov REST, no key |

### A.2 Structure-present / permit (fixer-upper)
- Structure-present: `improvval>0 AND yearbuilt` OR building-footprint (Regrid). Manufactured
  = structured `homeType`. Fixer/teardown = keyword classifier.
- Permit history: Shovels.ai / county Socrata (weak rural, address-keyed). Permit rules/zoning
  entitlement: NOT programmatic (per-county human step).
- Parcel providers: Regrid (gated per-record), ATTOM (~$95/mo entry), Rentcast (free 50/mo).

### A.3 Lego
- BrickLink API (free, OAuth-static creds, ~5k/day): Price Guide sold+current per set.
- Brickset API (free key): MSRP + piece count + retirement flags.
- BrickEconomy API (free, ~100/day): retired premium / growth / retiring-soon.
- Listings: eBay Browse (+ Feed API bulk); BrickLink native; FB/clearance scrape-only.
- Verdict: clean free API valuation path.

### A.4 Nike
- StockX Public API (OAuth, approved) — market-data-by-size effectively non-functional.
- KicksDB (paid wrapper ~EUR29-79/mo) — StockX+GOAT per-size pricing; the practical feed.
- GOAT/SNKRS — closed/adversarial. eBay Browse — sanctioned surface, structured size/style
  aspects + Authenticity Guarantee flag.
- Verdict: per-size valuation is paid-wrapper/scrape; eBay for the sanctioned deal-watch.

### A.5 OSS projects (reuse assessment)
- **changedetection.io** (Apache-2.0, 32k*): reuse as-is behind REST API for scrape-only sources.
- **PriceBuddy** (GPL-3.0): lift the JSON `scrape_strategy` enum-dispatch *design*.
- **Discount-Bandit** (license None): lift the item<->listing model *design*.
- **openclawshopping** (MIT, 1*): non-functional demo; skip.

### A.6 Cars / electronics
| Source | Bucket |
|---|---|
| Craigslist cars (`/search/cta?format=rss`) | (a) RSS — reuse glovebox RSS connector (native feed flaky; OpenRSS fallback) |
| Bring a Trailer (`/feed/`, `/category/<name>/feed/`) | (a) RSS |
| r/hardwareswap, r/homelabsales | (a) RSS ONLY — `.json` blocked (403) since ~2026-05-30; `.rss` throttled ~1 req/min/IP -> slow poll |
| Slickdeals (`feeds.feedburner.com/SlickdealsnetFP`) | (a) RSS |
| eBay Browse (Motors + electronics) | (b) free OAuth client-credentials, 5k calls/day, condition enum; sold comps gated (Marketplace Insights) |
| Best Buy Developer API | (b) free key; price + per-store availability |
| Amazon | (c) PA-API EOL 2026-05-15 (Creators-API keeps 10-sales gate) -> Keepa ~EUR49/mo |
| Cars.com, AutoTrader, CarGurus, Cars & Bids, Newegg, Micro Center | (d/e) scrape-only (Akamai/Cloudflare) |
| Facebook Marketplace (vehicles) | (e) closed -> (c) Apify ~$49/mo |

### A.7 Groceries / local consumables (Class A)
| Source | Bucket |
|---|---|
| **Flipp / backflipp** (`backflipp.wishabi.com/flipp/items/search?q=&postal_code=`) | (c) unofficial JSON, no auth, postal-code-scoped, ~400+ chains' circulars — **the pragmatic aggregator / primary trigger** |
| **Kroger Products API** | (b) official OAuth client-credentials + `filter.locationId` -> everyday price+availability; loyalty needs user auth — **the one clean official path** |
| Target Redsky | (c->d) unofficial JSON, tightening/IP-blocking |
| Walmart.io | (b) affiliate-gated (delegated-access keys retire 2026-07-30) |
| Safeway/Albertsons for U | (c) private mobile API (Okta JWT), coupons not shelf price |
| Instacart | (e) closed for price reads |
| OFF Open Prices | (a) free crowdsource, but too sparse (US local) to rely on |
| Costco, local chains, farmers-market/CSA | (d/e) scrape / none — Flipp is the coverage layer |
Verdict: no clean free firehose. v1 = **Flipp backflipp (name-match a watchlist) + Kroger API where a Kroger-family store exists**.

### A.8 Homelab hardware
| Category | Valuation + access | Aggregator to leverage |
|---|---|---|
| Servers | spec/$ from eBay listing specs (no feed) | **LabGopher OFFLINE since late-2025**; RackRat = web-only successor, no API |
| **HDD** | **$/TB via PricePerGig + DatacenterDisk = FREE JSON APIs** (condition/warranty/PoH) | best-covered category; diskprices.com is scrape-only |
| DRAM ECC | $/GB; DRAMeXchange=contract not retail | none; eBay + retailer scrape |
| CPU | PassMark/$; cpubenchmark.net no API | Second Hand Silicon (web-only scrape) |
| GPU | perf/$ + used trend | Second Hand Silicon / BestValueGPU (web); one weak RapidAPI |
| UniFi | RESTOCK@MSRP; store.ui.com no stock API | **subscribe** to TrackaLacker/UIPing rather than build |
- eBay Browse = common denominator (condition enum 1000 New … 2000 Certified-Refurb … 2500 Seller-refurb … 3000 Used … 7000 Parts). **Sold comps gated** (Marketplace Insights, independents routinely denied); free sold data only via Terapeak web UI.

### A.9 Marketplace seller-trust
| Platform | Trust signal access |
|---|---|
| **eBay** | (Open API) `feedbackScore`, `feedbackPercentage`, `sellerAccountType`, other active listings via `filter=sellers:`. **No account-age** (retired with Shopping API). The only clean structured trust. |
| Facebook Marketplace | (Closed) all signals login-walled; paid scrapers only, ToS-breaking |
| Craigslist | (Heuristic) fully anonymized -> infer from metadata: ships-on-local, price z-score, cross-city dup, payment-rail keywords (Zelle/gift-card), no-meet |
| Cars.com/AutoTrader/CarGurus | (Closed/scrape) dealer-vs-private + ratings are UI-only |
| BaT / Cars & Bids | (Scrape) public results; CLASSIC.COM aggregates comps |
- **VIN legitimacy (autos):** vPIC free (decode+recalls, no history); NMVTIS title/brand/odometer via paid provider (VinAudit ~$1/report cheapest); NICB VINCheck free but web-only.
- **Reverse-image / stolen-photo:** Google Vision Web Detection ($3.50/1k, 1k free/mo) or TinEye API. **Cross-listing correlation we compute ourselves** from the collected corpus.

### A.10 Corrections to earlier body assumptions (from the re-run)
- **LabGopher is dead** (offline late-2025); the body's "ride LabGopher" is wrong -> RackRat (web-only) or rebuild on eBay Browse. **But HDD gained free APIs** (PricePerGig/DatacenterDisk) — strengthens HDD as the hardware reference adapter.
- **Reddit `.json` is blocked (403) since ~2026-05-30**; homelab/hardwareswap subs are **RSS-poll-only, throttled ~1/min/IP** — not the free JSON firehose assumed earlier.
- **eBay account-age is not available** in any current API (retired Shopping API) — seller-trust scorer must not depend on join-date.
- **eBay sold comps are gated** (Marketplace Insights, independents denied) — reconfirms the category-reference-over-eBay-solds valuation decision across ALL Class B categories, not just Lego/Nike.
