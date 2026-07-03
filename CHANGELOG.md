# Changelog

All notable changes to nagus are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[0.1.0]: https://gitlab.orac.local/agentic/nagus/-/releases/v0.1.0
