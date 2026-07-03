# nagus

A multi-category **acquisition / watch** subsystem: it monitors marketplaces and
listing sources, sanitizes untrusted listing content behind the glovebox
boundary, normalizes it, enriches it with category-specific signals, scores it,
and **surfaces** good matches -- both as pushed alerts (quiet inbox + strong-match
ping) and as a read-only `search_items` query endpoint agents can ask questions
against.

It finds and reports; it does not act. There is no auto-buy, auto-bid, or
auto-contact -- any action is a separate, human-gated path (eyes, not hands).

## Two product classes over one spine

- **Consumables** (recurring / hyper-local): groceries and local variable goods
  -- standing list vs. current local prices, periodic digest.
- **Durables** (rare "like to have"): land, disks, servers, GPUs, Lego, ... --
  criteria vs. incoming listing, deal-quality scoring, strong-match ping.

A "category" is a plugin bundle over a generic spine:

```
connector -> glovebox sanitize -> extract/tokenize -> normalize -> store
          -> hard-filter -> enrich (item / counterparty / risk facets)
          -> score -> surface {push: inbox+ping | pull: search_items}
```

## v1

Two reference adapters: **land** (free gov geo-enrichment: FEMA flood / USGS
elevation / USDA soil / USFWS wetlands / Census geocode, plus a parcel-data
adapter) and **HDD** (`$/TB` deal-watch via a free valuation API). eBay Browse is
the common-denominator listing connector for durables; Craigslist rides the
existing glovebox RSS connector.

## Design

See [`docs/design/2026-07-01-nagus-design.md`](docs/design/2026-07-01-nagus-design.md)
for the full architecture, source-access map, security model, and decision log.

## Boundaries

- **glovebox** owns connectors + sanitization + extract/tokenize.
- **nagus** (this repo) owns the spine, the item store, enrichment facets,
  scoring, category bundles, and the `search_items` MCP server.
- **openclaw** registers the MCP tool for agents and wires delivery.

## Development

Go 1.26. Primary CI is GitLab (`.gitlab-ci.yml`, `gitlab.orac.local`); the GitHub
mirror (`.github/workflows`) is a downstream public/release concern.

```
go vet ./...
go test ./... -count=1 -race
```

Issue tracking is [beads](https://github.com/steveyegge/beads) (`bd ready`), not
markdown TODOs.

## Deploying & releasing

nagus ships as the [`charts/nagus`](charts/nagus) Helm chart (sqlite or postgres
backend). See [`docs/DEPLOY.md`](docs/DEPLOY.md) for install, gitops (Flux),
storage backends, secrets, and how openclaw consumes the `/mcp` + `/watches`
surface, and [`docs/RELEASE.md`](docs/RELEASE.md) for cutting a tagged release.
Changes are recorded in [`CHANGELOG.md`](CHANGELOG.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
