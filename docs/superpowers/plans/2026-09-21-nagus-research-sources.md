# Research-derived wine/spirits sources (2026-09-21)

Source: the winery/distillery DTC data-source research report. Rules that
bind every connector here: honor robots.txt, descriptive User-Agent, gentle
request rates, never bypass login/age gates or member pricing, personal
non-commercial use, no paid APIs.

## Platform survey (nagus fingerprint, 29 West Coast producers)

Commerce7 18, OrderPort 3, Vinoshipper 1, Shopify 2 (distilleries), AMS 2,
unknown/wordpress 3. Several producers moved off AMS/OrderPort to Commerce7.

## Beads

| Bead | Scope | Status |
|------|-------|--------|
| nagus-b0u | `nagus fingerprint DOMAIN...` platform detector | done (this branch) |
| nagus-tub | Vinoshipper connector (`/json-api/v2/wine-list?id=`) | done (this branch) |
| nagus-oc4 | Commerce7 connector (public `product/for-web`, `tenant` header) | done (this branch) |
| nagus-29b | fallback for non-feed stores: OrderPort HTML reader | done (this branch); AMS still unhandled |
| nagus-390 | Dynamics 365 Commerce connector (Chateau Ste Michelle; embedded LISTPAGESTATE, ?skip=N, 10s crawl delay) | done (dynamics365 branch) |
| nagus-cux | Shopify new-release/restock signals, sitemap lastmod diffing | open |
| nagus-0ek | TTB COLA new-label feed | open; ttbonline.gov fails TLS verification -- investigate chain, never bypass |
| nagus-t9n | WA in-state distilleries (spirits) | open |

## Design notes

- Shared HTTP: `internal/connector/webfetch` (UA, Retry-After on 429 capped at
  2m, non-200 is an error).
- Structured source fields (vintage, varietal, wine_type, bottle_ml) override
  title parsing in the wine extractor.
- A seller-published ships-to list (Vinoshipper `ships_to`) can only NARROW
  `ship_legal_to`, never widen it past the channel/origin rules.
- New config fields: `vinoshipperAccount`, `commerce7Tenant`, `catalogPath`.
- Deploy new sources without `lwinStamp` until their shadow matches are
  reviewed.

## Progress

- [x] Connectors + tests; live dry run: tablas-creek 80/80, kiona 23/23,
      hedges 21/22 stored, all WA-legal.
- [x] MR !9 merged (connectors), MR !10 (title beats stale structured
      fields; pgtest template0 fix)
- [x] Chateau Ste Michelle (user request): D365 Commerce connector; live
      dry run 125/132 stored (7 multi-bottle sets skipped), all WA-legal.
- [ ] MRs merged, image built
- [ ] gitops: sources deployed in shadow mode, ingest verified
- [ ] nagus-cux, nagus-0ek, nagus-t9n
