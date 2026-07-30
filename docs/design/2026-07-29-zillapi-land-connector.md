# Design: Zillapi land connector (replaces the retired Craigslist RSS source)

- **Status:** proposed
- **Bead:** nagus-hla (replaces nagus-hh5)
- **Supersedes:** the Craigslist tier-(a) RSS land source in
  `docs/design/2026-07-01-nagus-design.md` (see the amendment at the top of that doc)
- **Repo:** nagus (`internal/connector/zillapi`) -- see "Why nagus, not glovebox" below
- **Upstream contract:** OpenAPI 3.1 at <https://zillapi.com/openapi.json>,
  docs at <https://zillapi.com/llms-full.txt>, base URL `https://api.zillapi.com`

## Why we need this

Craigslist retired the `?format=rss` search feed the old land connector read.
Every URL carrying `format=rss` now returns their WAF block page (HTTP 403) while
the plain HTML search returns 200 -- an endpoint retirement, not an IP or
User-Agent block. Craigslist's ToU prohibits automated collection and
circumventing access controls, so there is no compliant replacement fetch path.
The connector was deleted (nagus-hh5) and land is currently surface-only.

Zillapi is the tier-(c) thin-API option the original design already named for
land ("Zillow land | paid wrapper -- Zillapi ~$5/mo (search includes lots)").

## Why nagus, not glovebox

The repo boundary is decided by **mechanism**, not by the word "connector":

- glovebox's `Connector` is `Poll(ctx, checkpoint) error` -- a side-effecting
  poller feeding glovebox's own ingest/staging/routing, returning no data. Its
  ~20 connectors are all personal/identity feeds (gmail, gcalendar, gdrive, imap,
  jira, notion, linkedin, outlook, schoology, steam, bluesky, github, gitlab,
  arxiv, hackernews, semantic-scholar, rss) atop `internal/subject` + `routing` +
  `staging`. It is built for per-subject sanitization of a person's data. A
  public land listing has no subject.
- nagus's `listing.Connector` is `Fetch(ctx) ([]listing.Raw, error)` -- it returns
  typed listings to the spine (normalize -> hard-filter -> enrich -> score).
- Precedent: `internal/connector/ebay` (the existing thin-API connector) lives in
  nagus, as did craigslist.

Craigslist was in glovebox's orbit **only because it was RSS** and glovebox owns
the generic RSS connector. A bespoke marketplace JSON API is not that.

Glovebox still owns the sanitize boundary (`POST /v1/sanitize`, nagus-9ib --
nagus runs `sanitize.Passthrough` today). Fetch is nagus's; body sanitization is
glovebox's.

## THE CONSTRAINT THAT SHAPES EVERYTHING: billing is per RESULT, not per call

From the docs, verbatim: *"One credit equals one property record returned."*
`/v1/search` is **1 credit per result returned (min 1)**. Failed calls are free.

| Plan | Credits | Rate | Notes |
|---|---:|---:|---|
| Free | 100 **one-time** | 20/min | does not renew; no top-ups |
| Monthly $5 | 1,000/month | 200/min | top-ups available |
| Annual $54 | 12,000/year | 300/min | top-ups valid through the term |

Concurrency is **1 in-flight request per key** (extra calls serialize, they are
not rejected).

This breaks the polling model nagus uses for eBay. eBay re-ingests everything
each cycle and refreshes `SeenAt`; that is free-ish and required by eBay's 6h
content-age rule. With Zillapi, **re-seeing the same listing costs a credit every
time**. A naive 30m poll returning 50 lots would be 50 x 48 = 2,400 credits/day.
That is ~2.4x the entire monthly plan, per day.

Consequences, which are design requirements and not tuning suggestions:

1. **Poll daily, not half-hourly.** `intervalMinutes: 1440`.
2. **Ask only for new listings.** `filters.daysOnZillow: "1"` (or `"7"`) so each
   poll bills for genuinely new lots rather than the whole standing inventory.
3. **Cap results explicitly.** `maxItems <= 50` also keeps the call synchronous
   (>=51 returns 202 + a job, which the connector would have to poll).
4. **Push the acreage/price window server-side** via `filters.lotSize` and
   `filters.price`. This narrows results *before* billing -- it is a cost control,
   not just a filter. It also matches the spine's filter-before-enrich rule.
5. **Land must not depend on re-ingestion for freshness.** The per-source
   `DeleteStale` purge exists for eBay's content-age obligation; a Zillapi land
   source must not be purged on a short window, because we cannot afford to
   re-see items to keep them alive. Non-eBay sources are already exempt.
6. **Report the credit balance.** `GET /v1/me` and `GET /v1/usage` cost **0
   credits**, so the existing metrics surface can expose remaining credits for
   free -- the same shape as the eBay call-budget metric already in
   `handleMetrics`.

## Request shape

`POST /v1/search`, `Authorization: Bearer zk_...`

```json
{
  "filters": {
    "status": "for_sale",
    "bbox": {"west": -122.5, "south": 47.3, "east": -121.9, "north": 47.8},
    "homeTypes": ["lot"],
    "lotSize": {"min": 43560},
    "price": {"max": 150000},
    "daysOnZillow": "7"
  },
  "extractionMethod": "PAGINATION",
  "maxItems": 25,
  "async": false
}
```

**A search is anchored by a bounding box, never a place name.** A free-text
`location` alone returns `400 invalid_filters` and never hits upstream. So the
connector config takes a bbox (`west,south,east,north` decimal degrees), NOT a
city string -- a real difference from the old Craigslist `city` config.
`location` may be sent as a decorative label only.

## Response shape and the acreage question

`{"data": [SearchResultRow...], "meta": {"count": n}, "request_id": "..."}`

`SearchResultRow` (the search shape, lighter than the property detail record)
declares: `zpid`, `id`, `address` + `addressStreet`/`City`/`State`/`Zipcode`,
`price` (formatted string), `unformattedPrice` (number), `beds`, `baths`,
`area`/`livingArea`, `latLong`, `statusType`, `statusText`, `homeType`, `imgSrc`,
`detailUrl`, `hdpData`.

**Open question -- lot size is NOT in the declared search-row schema.** The spec
mentions lot size only as a *filter*; `hdpData` is `additionalProperties: true`
(loosely typed) and in real Zillow payloads its `homeInfo` often carries
`lotAreaValue` / `lotAreaUnit`, but the spec does not guarantee it. Two options:

- **A (chosen for v1):** rely on `filters.lotSize` to enforce the acreage floor
  **upstream**, and opportunistically parse `hdpData.homeInfo.lotAreaValue` /
  `lotAreaUnit` when present. Cost: 1 credit/result. Acreage may be unknown on
  the item, but the returned set is guaranteed to satisfy the window. nagus's land
  extractor already tolerates unknown scalars (it must emit unpriced land rather
  than drop it), so unknown-acreage items still flow.
- **B (rejected for v1):** chain `GET /v1/properties/{zpid}/facts` per row for
  exact `resoFacts` lot size. Cost: doubles to 2 credits/result. Revisit only if
  A proves to leave acreage unknown often enough to hurt scoring.

**Also unconfirmed: the units of `filters.lotSize`.** It is a bare integer in the
spec with no unit stated. Zillow's own `searchQueryState` uses **square feet**, so
the connector will convert acres -> sqft (1 acre = 43,560 sqft) with that
assumption documented at the call site, and the fixture capture will confirm it.

## Config and credentials

Vault `eso/nagus/sources`, property `zillapi_key` -> env `NAGUS_ZILLAPI_KEY`,
wired in the chart like `land.rentcastExternalSecret` (`secretKeyRef`). The key
must never land in `config.json` -- that file is committed via the ConfigMap.

Source entry in the multi-source config:

```json
{"name": "seattle-lots", "category": "land", "type": "zillapi",
 "intervalMinutes": 1440, "maxItems": 25, "daysOnZillow": "7",
 "bbox": {"west": -122.5, "south": 47.3, "east": -121.9, "north": 47.8}}
```

`SourceConfig` gains zillapi-grouped fields (bbox, maxItems, daysOnZillow),
replacing the removed craigslist-grouped `city`/`clCategory`.

## Development plan (budget-aware)

The account is on the **one-time 100-credit free grant**, which at 1 credit per
result is roughly **two 50-result searches, ever**. So:

1. Write the connector and its tests against a **fixture first**, using the
   per-source `fixture` seam that already exists (`cmd/nagus/config.go` ->
   `SourceConfig.Fixture`, short-circuited in `buildConnectorForSource` exactly as
   the eBay connector does). Note the legacy `-ebay-fixture` *flag* is ignored in
   multi-source mode; only the per-source config field works.
2. Spend **one** capture call with `maxItems: 5` (5 credits, ~5% of the grant) to
   record a real response into `internal/connector/zillapi/testdata/search_lots.json`.
   That call also answers both open questions above (lot-size presence and
   `lotSize` filter units).
3. Iterate entirely offline against the fixture -- zero further credits.
4. Deploy with `intervalMinutes: 1440` and a small `maxItems`. Do not enable a
   sub-daily interval on the free grant.

Runtime needs the $5/month plan; the free grant is a development budget, not an
operating one.

## Test plan

- Fixture-driven unit tests (offline, no network, no credits): mapping of the
  captured response to `listing.Raw`, including a row with missing lot area, a
  row with no price, and a malformed/empty `data` array.
- `SourceID()` returns `zillapi:<name>` (per-source identity, the nagus-08k fix;
  freshness purge is scoped by SourceID).
- Auth: request carries `Authorization: Bearer <key>`; a missing key is a
  configuration error, not a silent no-op.
- Error mapping: 400 `invalid_filters` (bbox missing), 401, 429 `rate_limited`,
  and the `{error:{code,message,request_id}}` envelope surface as distinguishable
  Go errors; the ingest loop's per-source isolation keeps a failure from
  affecting other sources.
- A 202 (async) response is reported as an explicit unsupported-mode error rather
  than being silently treated as empty, since v1 keeps `maxItems <= 50` to stay
  synchronous.
- End-to-end offline: fixture -> Raw -> normalize -> hard-filter -> enrich ->
  `GET /search?category=land` returns a non-empty `items[]`.
- Trust boundary: `Title`/`Body` are UNTRUSTED free text passed through
  unsanitized (that is glovebox's job); price/lot scalars are trusted once parsed.
