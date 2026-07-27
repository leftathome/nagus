# nagus offer/product re-architecture

Status: DESIGN (approved to build the dgq slice first)
Date: 2026-07-26
Beads: nagus-dgq (slice 1, built first), nagus-q6u (slice 2, offer store),
nagus-7yq (slice 3, inquiries/eval), nagus-5n5 (slice 4, shopify),
nagus-d3v (status dashboard, cross-cutting)
Related: docs/design/2026-07-01-nagus-design.md (the spine this evolves),
docs/design/2026-07-26-shopify-connector.md (nagus-5n5 detail)

## Why

Two pressures converged:

1. **Deployment overhead.** Going live on both HDD and Land required two Helm
   releases (`nagus` + `nagus-land`) with duplicate PVC/Service/Deployment,
   because the chart serves ONE category per release (single `NAGUS_CATEGORY`,
   single source connector). Adding verticals multiplies deployments. (nagus-dgq)

2. **Sources are welded to evaluation.** Today a listing is extracted into a
   typed, category-specific `item.Item` at ingest time and only that is stored.
   A source that produces goods we do not currently evaluate (t-shirts today,
   maybe interesting tomorrow) is simply dropped. There is no way to cheaply
   accumulate a source's offers now and start evaluating them later without a
   cold start. And there is no separation between the good being sold and the
   act of selling it.

This document defines the target architecture and the order we build it.

## Entity model: offers are not products

The core correction. One physical good is sold by many sellers; identity belongs
to the manufacturer, not the seller.

- **Product** -- the actual good/service (e.g. "Samsung 990 PRO 1TB SSD").
  Identity from manufacturer brand + MPN/GTIN/model, independent of any seller.
  Specs live here. A product has its OWN lifecycle that precedes and outlives any
  offer: **development** (in the manufacturer's lab, not yet public) ->
  **announcement** (specs and approximate price made public, still unavailable)
  -> **available** (manufactured, boxed, shipped; offers appear) -> **EOL/retired**.
  An offer can reference an announced-but-not-yet-available product (a pre-order),
  so the offer lifecycle is NOT the same axis as the product lifecycle. The
  product lifecycle is quark's domain; nagus only references product state.
- **Offer** -- a specific listing at a specific source selling that product
  (eBay, Newegg, shopify:serverpartdeals each have their own offer for the same
  Samsung 990). Carries source, key, url, price, condition, seller, and its own
  lifecycle (appears -> discount -> clearance -> sold/retires), distinct from the
  product lifecycle above. MANY offers -> ONE product (N:1). A manufacturer that
  is also a vendor (Samsung sells the 990 direct) is just another source of
  offers; it does not collapse the offer/product distinction.
- **Inventory item** -- a product actually purchased and held.

## Service boundary (multi-repo)

This entity model spans services. Only the middle row is this repo.

| Service         | Owns                                                    | Authority                         |
|-----------------|---------------------------------------------------------|-----------------------------------|
| **quark**       | the **product catalog** -- canonical identity, specs, offer->product resolution; maintains products *before* purchase | identify / maintain               |
| **nagus** (here)| **offers/listings** -- monitor sources, track offer lifecycle, evaluate against criteria, surface the good ones | SURFACE ONLY, limited-to-no purchasing |
| **rom**         | **household inventory** -- what we actually purchased and hold | post-purchase custody             |

quark and rom are sibling services in their own repos (alongside glovebox,
openclaw, recognizer/archiver), each with its own spec cycle. This boundary
sharpens nagus's existing hard rule: nagus watches offers and points at good
ones; it never owns product truth and never buys.

### Product identity in nagus, before quark exists

quark does not exist yet. Decision: **provisional local grouping -- a limited
best-effort dedup of similar listings, not authoritative resolution.** Each offer
carries the raw product identifiers the source exposes
(`productHint{brand, mpn, gtin, model}`) AND a `provisionalKey` nagus computes by
normalizing (mpn, else brand+model). nagus groups offers across sellers by
`provisionalKey` so "cheapest Samsung 990 across N sellers" works now. We expect
it to be imperfect against real, messy, cross-source data and will tune it
empirically as sources come online -- it is NOT trying to be correct entity
resolution. When quark ships it supersedes `provisionalKey` with an authoritative
`productID`; the matching logic in nagus is deliberately throwaway.

## Target architecture (nagus)

```
SOURCES              OFFER STORE                    EVALUATION PROFILES        SURFACE
(always-on,          (durable, lifecycle,           (activatable, predicate-    (read-only,
 category-agnostic)   provisional product key)       selected, gate-at-eval)     search_items)

ebay        \      Offer{source,key,url,price,     / hdd  {predicate,          evaluated items
shopify:spd  +-->    seller,condition,             +       extract->filter->     (offer+product-
craigslist  /        lifecycle{first/last_seen,    |       enrich->score}        attrs+score),
                     price history,status},        \ land {...}                  grouped by
                     provisionalKey,                  (t-shirts: dormant,         provisionalKey
                     productHint{brand,mpn,gtin,model}  zero eval cost)           at query time
                     -- per-source retention:
                        eBay purge 6h (8.1b)
                          (summarize-decay pending
                           compliance validation);
                        others retain lifecycle
```

### Locked design decisions

1. **Offer != Product (N:1)**, related by `provisionalKey` now / quark
   `productID` later.
2. **Gate-at-evaluation, not gate-at-ingest.** The offer store holds UNTRUSTED
   bytes (offer free text). Nothing reads them as instructions; they are data at
   rest. The glovebox crossing + category extraction fire only when an active
   profile lifts an offer. This is what keeps dormant categories free: no
   glovebox calls, no extraction on goods we do not evaluate. Trust stays
   positional (design sections 4, 7, 13); the gate just moves to the point of
   use.
3. **Per-source retention, with summarize-and-decay.** Retention is a property of
   the source. It is not a binary purge-vs-retain; a source declares one of three
   policies:
   - `retain-full` -- keep the offer and its lifecycle indefinitely (Shopify,
     Craigslist; not eBay Content).
   - `purge` -- hard-delete after a window (the conservative fallback).
   - `summarize-decay` -- at the policy window, DROP the live listing detail but
     keep a coarse historical data-point: "this product WAS offered on <source>
     for X and sold for Y on <date>", where the date/price resolution COARSENS as
     it recedes into the past -- an RRD-style (round-robin) downsampling. Recent =
     precise; old = fuzzy aggregate.
   This is the INTENDED eBay policy, pending validation; **until validated, eBay
   uses `purge` at 6h** (the safe default). eBay License 8.1(b) forbids retaining
   eBay Content (the live listing) past ~6h; the intent is that a coarse "was
   offered / sold for / on" data point with decaying resolution is a summary, not
   retained Content -- but the exact summary schema and window are a COMPLIANCE
   JUDGMENT that must be validated against eBay's current policy before the eBay
   source is allowed to switch from `purge` to `summarize-decay`. The
   architecture must carry the policy per source and support summarize-decay for
   all sources; whether a given source is allowed to use it is a per-source
   policy decision, not an architecture one.
4. **Materialized evaluation.** Active profiles lift offers -> typed items into
   the EXISTING `store.Store`; surface/`search_items`/watch read typed items
   exactly as today. The offer store is a NEW layer IN FRONT; the existing item
   store and read path are unchanged. This preserves the deployed hdd/land
   surface while the offer layer is built additively.
5. **Ingest/surface split.** `pipeline.Pipeline` welds both halves sharing only
   the Store; with N sources feeding M category surfaces (N != M) the 1:1
   bundling stops matching reality. Split into `Ingester` (per source) and
   `Surface` (per category).

### Inquiries drive category activation

What earlier drafts called an "activatable profile" is really an **Inquiry** -- a
standing want held by a principal. An Inquiry is:

- **criteria** -- a specific product or a set of products in a category matching
  constraints. Examples span categories: "SSDs from Samsung, 1TB, M.2 connector";
  "size-L t-shirts with Frieren artwork"; "a 1-acre parcel within radius R of
  (lat, long)"; "a 1970-1974 Lotus Elan in running condition, continental US";
  "all issues of The Unbeatable Squirrel Girl, good-or-better"; "this specific
  Tag Heuer watch"; "men's size-12 Air Jordan 1 in reverse-bred colorway".
- **duration** -- how long to keep looking (an expiry), so a want does not search
  forever.
- **principal** -- who requested it, so we know whom to notify and on whose
  behalf we are looking.

An Inquiry's criteria imply a **category** (the extraction/scoring machinery for
that KIND of good: hdd, land, apparel, watches, comics, cars, sneakers...). A
category is DORMANT until at least one active (unexpired) Inquiry references it;
then it becomes ACTIVE and its machinery evaluates offers. This is the loose
coupling: **Inquiries (time-boxed principal wants) drive which categories are
active; categories provide the machinery; sources feed offers regardless.** No
active Inquiry for t-shirts -> the apparel category stays dormant (offers still
cached, zero evaluation cost) until someone asks.

The existing `watch.Watch` (saved query + notify threshold + audience) is a
PRIMITIVE Inquiry. Generalizing it means adding an explicit `duration`/expiry and
a `principal` (today's `Audience` is a routing tag, adjacent but not the same as
"who requested + for how long"). This generalization lands in slice 3; slice 1
(dgq) keeps categories active-by-config and the watch DATA MODEL unchanged (no
duration/principal) -- only category *dispatch* changes in slice 1 (see the
slice-1 detail, where `Watch.Category` becomes load-bearing).

## Interaction model

glovebox is the INBOUND ingest trust gate (it sanitizes untrusted listing
content on the source side); it is NOT the user/agent interaction surface. Those
are separate. A principal interacts with nagus two ways:

- **Look now (synchronous pull)** -- `search_items`: an agent asks "what matches
  X right now?" and gets ranked, read-only results. Already implemented as an MCP
  tool on `/mcp`.
- **Standing want (asynchronous)** -- register an Inquiry (criteria + duration +
  principal); nagus evaluates on its ingest cadence and reports matches. Today
  this is the static `watches.json` + `/watches`; slice 3 makes it dynamically
  registrable.

**MCP is the primary interface** (already the transport for `search_items`).
Standing-inquiry management (`create_inquiry` / `list_inquiries` /
`cancel_inquiry`) arrives as MCP tools in slice 3. Per the repo boundary, those
tools are REGISTERED and access-controlled in openclaw (openclaw owns the agent
runtime, per-agent/audience access, and delivery); they call nagus. **Working
assumption: nagus owns the inquiry store** (an Inquiry is a generalized
`watch.Watch`, and `watch.go` already lives here); the principal is an OPAQUE tag
to nagus. openclaw resolves the principal/audience and performs the actual
notify/ping -- nagus reports, it never delivers (eyes, not hands). This ownership
boundary is finalized in the slice-3 design.

- **No product-browsing website in nagus.** A human UI to browse offers/specifics
  would consume nagus's MCP/HTTP through a frontend/openclaw, not live here.
- **A2A/ACP deferred** -- MCP covers agent-to-nagus today (YAGNI until a concrete
  interop need).

### Lightweight status dashboard (human-facing, token-free)

Separate from the product surfaces: a tiny human-facing STATUS page served by
nagus, consultable without burning tokens (plain server-rendered HTML, no LLM in
the loop) -- the thing someone glances at to confirm nagus is running properly
without logging into Grafana. It shows only AGGREGATE operational signal, the
same family of data already exposed at `/metrics`:

- count of inquiries being tracked;
- count of products being watched (provisional product groups / stored items);
- basic per-connector info (source name, last fetch time, ok/error, eBay budget
  remaining).

Constraints: **no PII, no specifics** (no listing detail, no principal identity,
no criteria text) -- aggregate counts and liveness only. It is an operational
health view of nagus ITSELF, which is why it belongs here and not in a frontend.
A minimal version is feasible early (watch count + item count + per-source
connector status from the slice-1 multi-source loop) and gets richer as slices 2
and 3 add offers and inquiries. Filed as its own small cross-cutting bead
(nagus-d3v). **Slice 1 has no dependency on the dashboard and never delivers it:**
the dashboard bead may CONSUME slice-1 data, but it is never a slice-1
deliverable -- do not pull it into the dgq plan.

## Slices (build order)

Approved order: build the deployment merge (dgq) FIRST against the materialized
model, then add the offer store layer. Each slice keeps prod working.

| Slice | What | Bead |
|-------|------|------|
| **1. dgq -- multi-source/multi-category deployment** (BUILT FIRST) | Split ingest/surface; N sources x M categories in one deployment; mounted config; merge the two HelmReleases. Materialized typed items as today; NO offer store yet. | nagus-dgq |
| **2. Offer store + lifecycle + provisional key** | New `offer.Store` adapter (Memory+SQLite), `Offer` entity, ingest writes offers, per-source retention, provisional grouping. Additive. | (new) |
| **3. Inquiries + activatable evaluation (gate-at-eval)** | Generalize Watch -> Inquiry (criteria + duration + principal); active Inquiries activate their category; evaluation reads offers -> sanitize -> extract -> item; dormant categories = free. Extraction moves from ingest-time to eval-time. | (new) |
| **4. Shopify connector** | Generic products.json source, per-store, as one of N sources. | nagus-5n5 |
| *(sibling repos)* | **quark** (product catalog), **rom** (inventory) | separate specs |

## Slice 1 detail: dgq (the first build)

Materialized model, no offer store. Sources stay category-bound at ingest (there
is no offer store yet to defer to), but the config is structured so the binding
later becomes a profile predicate.

### `internal/pipeline` -- the split

- New `Ingester{Connector, Sanitizer, Extractor, Store, StaleAfter, Now, Logf}`
  with `Ingest(ctx) (IngestResult, error)` -- one instance per source. The
  freshness purge stays scoped by `Connector.SourceID()` (unchanged semantics,
  moved onto Ingester).
- The surface half becomes `Surface{Store, Filter, Valuate, Logf}` with
  `Surface(ctx, store.Query) (SurfaceResult, error)` -- one instance per
  category.
- `watch.Evaluate` retargets from `*Pipeline` to a single `*Surface` (the surface
  for that watch's category). `watch.EvaluateAll` must **dispatch each watch by
  its `Category` to the matching surface**, because a merged hdd+land deployment
  has watches spanning categories and each `Surface` bakes in one category's
  Filter/Valuate -- evaluating a `land` watch through the `hdd` surface would
  apply the wrong filter/valuation. So `EvaluateAll` takes the
  `surfaces map[string]*Surface` (or a `func(category) (*Surface, bool)`
  resolver), not one Surface; a watch naming an unconfigured category is a
  per-watch error (`watch %q: unknown category %q`). Today `Watch.Category` is
  effectively cosmetic (one wired pipeline); this slice makes it load-bearing.
- `IngestResult`, `SurfaceResult`, `Scored`, `Skip` types are unchanged; only
  their owning struct splits.

### `cmd/nagus` -- collections over one shared store

- `server` holds `ingesters []*Ingester` and `surfaces map[string]*Surface` over
  one shared `store.Store`.
- `runIngestLoop` runs ONE goroutine per source, each on its own interval
  (eBay 30m for the API budget; Shopify/Craigslist may differ), with per-source
  failure isolation: one source's ingest error is logged and the loop continues,
  never affecting another source. The existing eBay budget-exhausted back-off is
  preserved per-source.
- `handleSearch` dispatches by `q.Category` -> `surfaces[cat]` (the read path is
  already category-parameterized via the `category` query param). Unknown
  category -> 400. When the `category` param is ABSENT, there is no single
  default in a multi-category server: if exactly one category is configured, use
  it (preserves single-source ergonomics); if more than one, require the param
  (400 with the list of configured categories). A configurable
  `defaultCategory` in `config.json` may override this to pin one.
- `handleWatches` calls `watch.EvaluateAll` with the `surfaces` map so each
  saved watch is evaluated through its own category's surface (see the watch
  dispatch note above). This is part of the deployed read path the slice keeps
  working.
- `/metrics` iterates ingesters and emits the eBay budget gauge for any eBay
  source (today it type-asserts the single pipe's connector).

### Config -- mounted file, not scattered env

A source LIST does not fit flat env vars. nagus already loads watches from a
JSON path; reuse that pattern with a ConfigMap-mounted `config.json`:

```json
{
  "sources": [
    {"name":"ebay","category":"hdd","type":"ebay","query":"internal hard drive","intervalMinutes":30,"secretRef":"ebay"},
    {"name":"cl-seattle","category":"land","type":"craigslist","city":"seattle","clCategory":"rea","intervalMinutes":60}
  ],
  "categories": {
    "hdd":  {"minCapacityTB": 8},
    "land": {"minAcreageAcres": 1, "budgetCents": 0, "rentcastSecretRef": "rentcast"}
  }
}
```

- `type` selects the connector builder (ebay | craigslist | shopify later).
- `category` binds the source to a category's extractor (slice 1 only; becomes a
  profile predicate in slice 3).
- `intervalMinutes` is the per-source cadence.
- `secretRef` / `rentcastSecretRef` name a per-source secret; the chart wires an
  ExternalSecret per referenced secret and injects it as env for that source.
- The existing flat env vars (`NAGUS_EBAY_*`, `NAGUS_CL_*`, `NAGUS_CATEGORY`)
  remain as the SINGLE-SOURCE fallback for back-compat and CLI/tests: if no
  `config.json` is provided, build one source from env as today.

### Chart

- `category` (scalar) -> `sources[]` + `categories{}`; render `config.json` from
  values into a ConfigMap and mount it.
- Wire one ExternalSecret per distinct `secretRef`; inject its keys as env.
- Collapse `helmrelease-nagus.yaml` + `helmrelease-nagus-land.yaml` into a
  SINGLE release running both sources/categories. One PVC/Service/Deployment.

### Prod migration

The merged deployment replaces the two. Validated on the fixture/offline path;
the live eBay and Craigslist blockers (nagus-9nx unauthorized_client; nagus-hh5
Craigslist 403) remain separate and unchanged by this slice. Scale-down of
`nagus-land` and removal of the second release happen as part of the cutover.

### Test plan (slice 1)

All automatable against `MemoryStore` (the reference contract), no money in the
loop:

- `Ingester` unit tests split from `pipeline_test.go`: fetch -> sanitize ->
  extract -> store, skip accounting, StaleAfter purge scoped by SourceID.
- `Surface` unit tests split from `pipeline_test.go`: query -> hard-filter ->
  valuate -> score -> rank; hard-filter-before-enrich ordering invariant.
- Multi-source ingest loop: two ingesters, one failing, assert the other still
  ingests (failure isolation) and per-source purge scoping.
- Multi-category surface dispatch: `handleSearch` routes by category; unknown
  category -> 400; absent category with one vs many configured categories;
  shared store returns category-scoped rows.
- Multi-category watch dispatch: `EvaluateAll` over a config whose watches span
  hdd and land routes each to its own surface (a land watch is NOT evaluated
  through the hdd filter/valuation); a watch naming an unconfigured category
  errors per-watch.
- Config loader: parse `config.json`, build ingesters + surfaces; env fallback
  path builds a single source when no file is present.
- `go vet ./...`, `go test ./... -count=1 -race`, staticcheck as the gate.

## Open items (deferred to later slices, not slice 1)

- Offer store schema and lifecycle transition model (slice 2): minimal v1 =
  first_seen, last_seen, last_price, price_min_seen (discount detection), status
  (active | gone); full price history later.
- Predicate language for profile selection (slice 3): start with source/type/
  keyword predicates; keep it declarative and small.
- Grouped surfacing by `provisionalKey` ("cheapest across sellers") -- store the
  key in slice 2; add grouped views when a consumer needs them.
- quark contract (product resolution API) and rom -- separate repos/specs.
