# Wine market signals: who owns what (2026-09-23)

Supersedes the comparables design drafted in MR !15
(`2026-09-22-retail-reference-comparables.md`, not merged). That draft built
market comparables and cross-seller score inheritance INSIDE nagus, keyed on
nagus's own LWIN resolver. Operator review caught two errors:

1. It treated a retailer that cannot ship to Washington as "reference only".
   Whether we can BUY a bottle depends on the destination; what the market
   CHARGES for it does not. The same listing is actionable for a gift or for a
   user in another state. Legality stays on the offer (`ship_legal_to`, now on
   every row) and is filtered per destination at the surface.
2. It crossed the quark/nagus seam. quark's design
   (`quark/docs/superpowers/specs/2026-08-30-quark-product-catalog-design.md`)
   is explicit: quark "identifies and maintains; never sees a price, never
   ranks"; D1 moves LWIN identity into quark; D4 says nagus's matching logic
   is "deleted rather than evolved"; D5 is hints in, id/specs out.

## The split

| Concern | Owner | Notes |
|---|---|---|
| Wine identity (LWIN resolution, adjudication tiers) | **quark** | D1 migration; quark has the `lwin` namespace but no LWIN catalog loader yet (its slice 3). |
| Critic scores as facts about a vintage | **quark** | A score belongs to the product, not an offer: the deferred "observed specs with provenance" slice (D5). |
| Producer as an entity, and its website | **quark** | A `Publisher` (manufacturer; corporate https references). |
| Extracting hints from a listing: producer, critic codes | **nagus** | Extraction. Sent to quark as resolution hints. |
| Market comparable: other sellers' offers for the same product | **nagus** | Valuation over the offer layer, which already carries quark's `ProductID`. Excludes the listing's own source. |
| Legality per destination; verdicts; watches | **nagus** | Unchanged. |
| Which stores to poll; the fingerprint tool | **nagus** | Acquisition. |
| Page scraping for catalog loaders; email offers | **glovebox** | Per quark's catalog-loaders table; nagus-239. |

## Order of work

1. nagus (this MR): extraction only -- bracketed critic codes (`[JS98][WA97]`,
   `[V90]`), `producerFromBody` hints for retailers whose Shopify vendor is
   useless ("WINE"), the `producer` attribute, and `producer` +
   `ship_legal_to` on rows.
2. quark: an LWIN catalog loader and the resolver migration, so wine offers
   get quark ProductIDs.
3. nagus: comparables over offers grouped by quark ProductID.
4. quark: critic scores as product specs with provenance; nagus valuation
   reads them.

## Lessons kept from the draft

- Only a HIGH-confidence identity may join offers: joining on a
  mid-confidence LWIN guess priced a $43 Ridge Zinfandel against a $5.99
  listing. (quark's adjudication tiers are where this belongs.)
- A retailer needs a producer hint to be identifiable at all.
- A Shopify page cap can masquerade as a data finding: the first overlap
  measurement read 783 of Bottle Barn's 7,357 products and concluded "zero
  overlap"; the full catalogue carries Ridge, Turley, Bedrock and Cayuse.
- Report a comparable always; set a verdict only on real evidence (several
  listings, or two independent sellers agreeing). A market rate never claims
  quality.
