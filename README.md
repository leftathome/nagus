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
the common-denominator listing connector for durables; land acquisition is
Zillapi (`internal/connector/zillapi`), which replaced a Craigslist RSS source
after Craigslist retired that feed. Zillapi bills per RESULT returned, so a land
source polls daily with a bounded result cap -- see
`docs/design/2026-07-29-zillapi-land-connector.md`.

A third adapter, **wine**, runs on a $0/month data stack: LWIN (Creative
Commons) for canonical identity -- resolved by quark, which owns product
identity, from each listing's producer and title -- critic-score normalization + a hedonic
log-price value model for quality/value, and a data-driven ship-legality
constraint layer covering ~110 jurisdictions worldwide -- each source declares
its channel (producer/retailer) and origin (`US-WA`, `CA-BC`, `FR`), a
per-destination rules table computes where it may legally ship, and a watch
picks its destination jurisdiction: the household's own, or a gift
recipient's abroad (fail closed throughout). See
[`docs/design/2026-08-30-wine-category.md`](docs/design/2026-08-30-wine-category.md).

## Design

See [`docs/design/2026-07-01-nagus-design.md`](docs/design/2026-07-01-nagus-design.md)
for the full architecture, source-access map, security model, and decision log.

### Agent surface (MCP, `/mcp`)

All read-only. `search_items` (ranked search), `get_item` (one listing),
`new_deals` (listings first seen -- or whose price dropped -- since a time the
agent passes; nagus keeps no cursor), `list_watches` (the household's
standing watches and their live counts), `price_history` (one product's
price changes across sellers: current and all-time low, change over N days,
per-seller timelines), and `deal_submission_spec` /
`deal_submission_status`. With the offer layer on, listings whose offer has
EXPIRED (not seen at the source for 3 polls) are never returned by search or
watches, and every row carries `first_seen_at`, `last_seen_at`,
`price_changed_at`, `lowest_price_cents`, `offer_status` and `expires_at` with
an `expires_basis` that says whether the end time is the seller's
(`listing_end`), a deal email's read window (`email_window`) or nagus's
estimate (`not_seen_3_polls`). Design:
[`docs/design/2026-10-03-new-deals-offer-timing.md`](docs/design/2026-10-03-new-deals-offer-timing.md);
the Caspar skill snippet: [`docs/new-deals-skill.md`](docs/new-deals-skill.md).
Price history (the `offer_price_history` table, its retention and the
`price_history` tool):
[`docs/design/2026-10-03-offer-price-history.md`](docs/design/2026-10-03-offer-price-history.md).

Household humans and agents can also email a deal nagus cannot see itself
(in-store, image-only newsletters, tips) as one JSON object per line:
[`docs/deal-submission.md`](docs/deal-submission.md).

Image-only retailer newsletters on the same mailbox (Newegg, wine.com, Total
Wine) are read automatically by a vision model (`promo-vision-v1`): product
images are fetched from the retailer's own CDN, each is read twice with two
independent prompts, a product is kept only when both reads agree on the
price, and only a clean canonical product URL is stored. Rows from these
sources carry `offer_origin: "email_image"` and `submitted_by:
"retailer:<name>"` so agents can treat them as lower trust:
[`docs/design/2026-10-16-promo-vision.md`](docs/design/2026-10-16-promo-vision.md).

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
scripts/lint.sh   # golangci-lint v2 (.golangci.yml); same command as the CI lint job
```

CI runs all three (plus govulncheck) on every MR and on main, so there is no
need to run the whole suite or the linter by hand before pushing; run the
tests for the packages you changed and let the pipeline do the rest.

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
