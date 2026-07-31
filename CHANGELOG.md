# Changelog

All notable changes to nagus are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[0.3.0]: https://gitlab.orac.local/agentic/nagus/-/releases/v0.3.0
[0.2.0]: https://gitlab.orac.local/agentic/nagus/-/releases/v0.2.0
[0.1.0]: https://gitlab.orac.local/agentic/nagus/-/releases/v0.1.0
