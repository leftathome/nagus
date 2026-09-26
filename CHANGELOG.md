# Changelog

All notable changes to nagus are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Upgrade notes

- **Deploy quark with its LWIN catalog first** (quark QUARK-04: chart 0.6.0,
  `catalogs.lwin.url` set, first load complete). This release stops resolving
  wine identity in nagus; until quark answers wine name hints, opted-in wine
  offers are recorded refused (quark before QUARK-04 refuses category `wine`)
  and are re-offered when quark's catalog generation advances.
- **Wine items no longer carry `CanonicalID` or the `lwin_*` attributes.** Items
  are re-extracted on each ingest, so previously stamped LWIN-11 ids disappear
  from items within one ingest interval (720m for the producer stores). Wine
  identity is now quark's product id on the OFFER, surfaced as a row's
  `product_id` like every other category.

- **`NAGUS_LWIN_STAMP` now renders whenever `lwin.stamp` is set**, with no
  need for `lwin.url` (chart 0.12.0). Before, the chart emitted it only
  inside the `lwin.url` block; the current production values set both, so
  nothing changes there, but a values file that sets only `lwin.stamp` now
  takes effect.
- **Wine rows carry `comparison_key` and `vintage_status`.** A wine's
  `product_id` is the wine across all its vintages; compare offers on
  `comparison_key` instead (see Added).

### Removed

- **The LWIN resolver and mirror** (`internal/identity/lwin`,
  `internal/refdata`, `cmd/nagus/lwin.go`) -- moved to quark (quark QUARK-04,
  design D1), not forked. With them go the `nagus_lwin_*` metrics, the wine
  first-ingest wait for the dictionary (nagus-0k0), and the chart's
  `NAGUS_LWIN_URL`/`_CACHE`/`_MAX_AGE` env (chart 0.12.0; the `lwin.url`,
  `cachePath` and `maxAge` values are retired but still accepted). nagus logs
  once if those variables are still set.

### Changed

- **`lwinStamp` now selects wine sources for quark name hints.** With the
  global `lwin.stamp` / `NAGUS_LWIN_STAMP` on, an opted-in wine source's
  offers carry `brand` = declared producer and `text` = sanitized title (and
  nothing else) to quark, which resolves them against its LWIN catalog and
  returns a product id only for an auto-band match (new route `fuzzy`;
  `adjudicate` records refused). The same three sources that stamped before
  are identified now. `pipeline.Ingester.NameHintProducer` carries it. The
  hint is built from what the glovebox gate passed (the sanitized title and
  producer); when the gate does not pass a listing -- refused, or glovebox
  down -- the offer keeps the hint and resolution it already had, so an
  outage no longer resets resolved wine offers.
- **Vintage-aware comparison key** (`offer.ComparisonKey`). quark states per
  wine product whether its vintage matters (`vintage_mode`: `vintage`,
  `non_vintage`, `unknown`, from the LWIN export's VINTAGE_CONFIG); nagus
  stores it with the offer's resolution (additive `vintage_mode` column,
  Postgres `ADD COLUMN IF NOT EXISTS`, SQLite `table_info`) and keys
  comparisons on (product, vintage) for vintage wines, on the product alone
  for non-vintage blends (Bollinger Special Cuvee "NV" and "disgorged 2019"
  are one thing to compare; La Grande Annee 2014 and 2015 are not), and
  gives a vintage wine listed with no year no key at all. `unknown` behaves
  as `vintage` unless the title says NV. Rows expose `comparison_key` and
  `vintage_status`. An explicit NV marker is wine evidence at extract
  (attribute `nv`) only beside another wine cue (brut, cuvee, champagne,
  cremant, cava, prosecco, sparkling, rose, blanc de blancs/noirs, or a
  colour or varietal), never as a state after a city (", NV") or a company
  suffix ("N.V. Beer"); the merchandise list gains key chains, foil cutters,
  stoppers, gift boxes, pickup fees, shipping charges and olive oil. A title
  that says NV on a product quark calls `vintage` gets no key
  (`conflict_nv`). A year right after disgorged, bottled, Est., since or
  anniversary is not taken as the vintage.
- **Fortified wine is wine; culinary is its own rejection.** Port, sherry,
  madeira, marsala and their styles (colheita, LBV, fino, manzanilla,
  amontillado, oloroso, PX) are wine evidence without NV -- but not beside a
  cask word or on a spirit/beer title ("Sherry Cask Bourbon"), and "port"
  the place or connector is set aside. Culinary products (vinegar, cooking
  wine, cake, cheese, jelly, olive oil; a fortified word on a sauce, jam,
  trifle mix, fudge or chocolate) are rejected as `wine.ErrCulinary`, not
  merchandise (`wine.ErrMerchandise`): both wrap `wine.ErrNotWine` and are
  told apart in the extract skip reason, reserved for a possible future
  grocery category.
- **Appellations are wine evidence** (nagus-tmr). Old World wines named by
  place ("Castello di Ama Chianti Classico", "Barolo Vietti", "Sancerre
  Vacheron", "Etna Rosso Benanti") were dropped as not-wine when the title
  carried no year, grape or colour word. A static table generated offline
  from the Liv-ex LWIN export (`tools/genappellations`; REGION/SUB_REGION
  names of at least 20 live wines, CC BY 4.0, attributed in the generated
  file) plus a hand supplement (Etna, Brunello, Amarone, Muscadet, ...)
  now counts: specific appellations alone, broad regions and New World
  AVAs (Burgundy, Tuscany, Napa Valley) only beside a classification token
  (DOC, AOC, Grand Cru, Riserva, ...) or a bare colour word. Bare "Red",
  "Rouge", "Rosso", "Tinto", "Blanco" (and Bianco, Blanc, White, Rosado,
  Rosato) are the colour beside an appellation or NV, and nowhere else.
  A broad name's colour word must sit next to it (at most one word between)
  in a title with no object noun ("Burgundy Blanc Throw Pillow" is not
  wine). Appellations that double as places or words (Vesuvio, Santorini,
  Hermitage, Douro, Macon, Montrachet, Brunello, ...) are broad. Cask
  finishes, spirits, events and travel, printed matter and foods withdraw
  the cue ("Sauternes Cask Finish", "Barolo Tasting Dinner", "Douro River
  Cruise", "Chianti Salami"). An NV marker and a bare colour need a third
  cue ("Red NV" is not wine). nagus still loads nothing from LWIN at
  runtime.
- **Culinary is a head-noun rule** (nagus-tmr). A title's food noun
  (vinegar, cake, cheese, jelly, jam, preserves, marmalade, chutney,
  compote, syrup, honey, mustard, olive oil, cooking wine) makes it
  culinary unless a varietal, appellation, fortified style or colour
  keyword or bare colour word FOLLOWS it: "Sherry Vinegar", "Chardonnay
  Jam" and "Madeira Cake" are culinary; "Cake Bread Cellars Chardonnay",
  "Jelly Roll Zinfandel", "Vinegar Hill Syrah" and "Mustard Seed Red 2020"
  are wine. A food word in a producer name ("Butter Chardonnay by JaM",
  "JaM Cellars Butter") is not a food. A year never rescues one, from the
  title or the body. A food in a wine pack or bundle ("Spritz Pack w/ ...
  Fruit Syrup") is merchandise; a food set or duo stays culinary; mead
  ("Honey Wine") is plain not-wine.
- **Port styles and guards** (nagus-tmr). Oak or wood before a port word is
  a style ("Oak Aged Port", "Wood Port"), and after a named style too ("Old
  Oak Tawny Port", "Tawny Port, oak aged"); cask, barrel and finish still
  withdraw it. A spirit word in a producer name right before the style is
  the producer ("Porter Creek Tawny Port", "Gin Lane Port"). "Tawny-Port"
  counts. "Angelica" (California's fortified dessert wine) is a fortified
  style beside a bottle size, "dessert wine" or a declared wine type (alone
  it is a herb or a name); "2020 Angelica" is wine by its year. "Port
  Ellen" and "Port Askaig" are Scotch.
- **Object merchandise is not rescued by a grape** (nagus-tmr). Towels,
  charms, soap, flutes and tools are merchandise even with a varietal, a
  year or a pack count ("Merlot Tea Towel", "2-Pack Champagne Flutes"),
  unless a bottle size or an explicit pack of wine is in the title -- when
  the object is the title's head noun ("Charm City Syrah 2020" is wine).
  Tees, socks, stickers, magnets, mugs, perfume, sweaters, posters, prints,
  paddles and jerseys (not New Jersey) follow the same head-noun rule:
  "Sancerre Tee" is merchandise, "Sweater Weather Red Blend 2022" wine.
- **Bare "Cabernet" is a varietal** (red), after every other grape.
- **A Shopify SKU is no longer sent to quark as the part number** (quark
  QUARK-02; operator-approved re-key). serverpartdeals SKUs carry seller
  segments after the manufacturer part number (`HUS726040AL4215-DELL_DELLG13`,
  `00JHTD_DELLG14_SR_12`), so one drive became many quark products -- 266 keys
  over 61 base part numbers on the live store -- and no listing title could
  ever match a key. The MPN is now the longest title token the SKU starts
  with, else the SKU's part-number-shaped leading segment, else none. Offers
  whose hint changes are re-resolved by quark automatically; the old
  SKU-keyed products stay in quark unreferenced (nothing is merged or
  deleted). The committed serverpartdeals baseline moves from 35 to 25
  products: ten groups were each one drive in different caddies, audited.

### Fixed

- **A store's own non-wine declaration beats text evidence** (nagus-tmr).
  Broc Cellars' "June Taylor Mission Fig + Angelica Jam" (product_type
  Pantry, tags merch and pantry) was stored as a 2020 wine because its
  description names "our 2020 Angelica dessert wine" (production,
  2026-09-25; pre-existing since at least c4b6e98). The Shopify connector
  now carries a product's tags as the `tags` aspect for WINE sources only
  (other categories' listings, e.g. serverpartdeals' long drive tags, do
  not reach the glovebox gate or the stored offer), and the wine extractor
  rejects a listing whose product_type is Pantry, Food or Grocery
  (culinary) or Merch, Merchandise, Apparel, Gift Card(s), (Wine)
  Accessories, Glassware, Books, Events, Tickets or Membership
  (merchandise), or which is tagged pantry or food (culinary) or merch,
  merchandise, apparel or gift card (merchandise); declared both, the title
  decides (a food title is culinary, anything else merchandise). Exact
  words only: a wine tagged "gifts" or "holiday" stays wine. Commerce7 and Vinoshipper already
  skip non-wine types; OrderPort publishes none.
- **serverpartdeals was never walked to its end, so tail rows never
  refreshed** (nagus-bu2). The store's catalogue is more than 10,000 products
  (40+ full pages at 250, measured 2026-09-25) and hard drives are spread
  across all of them, so the 12-page cap reached 147 of the 260 in-stock
  drives. Rows past the cap kept their pre-re-key seller-SKU hints and quark
  ids, and offer expiry was skipped on every run. A shopify source can now
  set `collections` (a list; `collection` is a single-entry alias), which
  walks each `/collections/<handle>/products.json` instead of the whole
  catalogue, deduping products by id; the fetch is complete only when EVERY
  collection's walk is. No single serverpartdeals collection covers the
  drives: `all-hard-drives` (260) misses the 7 in-stock drives typed
  "HDDs > ..." (live offers today), which only `hard-drives` (266) holds,
  and `hard-drives` misses one that only `all-hard-drives` holds; together
  they are 267 products, every in-stock drive (measured 2026-09-25), in 4
  pages. Every row is re-ingested with its manufacturer-part-number hint (a
  changed hint resets the offer's resolution, so quark re-resolves it), and
  expiry runs again. SourceKeys and product URLs do not change. Needs the
  gitops value `collections: [all-hard-drives, hard-drives]` on the
  serverpartdeals source.
- **An empty collection is never a complete walk.** A collection that
  returns no products on page 1 (emptied, hidden or renamed: Shopify
  answers an unknown handle with an empty list) makes the fetch incomplete
  and logs a warning, so it cannot expire every offer the source holds.
- **A failed Shopify fetch resets completeness** at its start, as commerce7
  and vinoshipper do, so a fetch that errors part-way never leaves the
  previous run's "complete" standing.
- **Shopify fetches pace themselves**: a 3s courtesy pause before every page
  after the first (`shopify.Config.PageDelay`), so a multi-page walk is a
  trickle, not the burst that trips a store's limiter. One-page stores never
  wait.
- **The truncation warning counts store products read and listings kept
  separately.** "143 products fetched" was 3000 store products read, of
  which 143 variants passed the allow-filter; the smaller number hid how far
  short of the catalogue the walk stopped.

### Added

- **Title text hints for quark** (quark QUARK-02). A source may set
  `quarkTextHints: true`: when it states no product identifiers (eBay search
  results carry the part number only in the title), the listing TITLE is sent
  to quark as hint text, and quark links it to a product only if the title
  names a key quark already holds. The title is attached only after the
  glovebox gate passed the listing (offers are otherwise recorded before the
  gate), only when every structured hint field is empty, and the gate is still
  called once per listing. Offers gain an additive `hint_text` column
  (Postgres `ADD COLUMN IF NOT EXISTS`, SQLite `table_info`); a hint without
  text keeps its exact old fingerprint, so nothing re-resolves on upgrade.
  quark's `text` route stamps resolved; `unmatched` stamps refused, retried
  when quark's catalogue generation grows. Enable only after quark accepts
  `text` (quark MR !6): an older quark rejects the unknown field.

## [0.5.1] - 2026-09-24

### Fixed

- **A search limit now applies after ranking, not before** (nagus-cb8). The
  surface passed the caller's `limit` to the store, which returned the first
  N rows in storage order; the hard filter then dropped most of them and
  scoring ranked what was left. On the live hdd corpus, `limit=3` returned 0
  rows, `limit=10` returned 1, and a top-N could omit the best deals
  entirely. Every candidate (up to `DefaultMaxCandidates`, 5000; reaching it
  is logged) is now filtered, valued and ranked, and only then truncated.
  `matched` and `filtered` describe the whole candidate set. This affects
  `/search`, MCP `search_items`, and watches with a `limit`. Found by the
  v0.5.0 production smoke test; present since 2026-07-27.

## [0.5.0] - 2026-09-24

### Upgrade notes

- **Chart 0.5.1 -> 0.10.0; appVersion is still `0.4.0`.** In order: 0.6.0 adds
  the quark resolution pass, 0.7.0 adds the ServiceMonitor and PrometheusRule,
  0.8.0 adds LWIN mirroring, 0.9.0 adds the glovebox sanitize gate values, and
  0.10.0 adds the token-as-file mount and the sanitize alerts. Every new feature
  is off by default (`quark.url`, `glovebox.sanitizeURL` and `lwin.url` all
  default to empty), so a release that sets none of them renders its old
  behavior. The exceptions are the monitoring objects and the memory limit,
  listed next.
- **`serviceMonitor.enabled` and `alerts.enabled` default to `true`.** They
  render only where the prometheus-operator CRDs exist, so a cluster without
  them sees no change. Thresholds you can tune: `alerts.quark.stallSeconds`
  (1800), `alerts.quark.for`, `alerts.sanitize.dropRatio` (0.9),
  `alerts.sanitize.minListings` (20) and `alerts.sanitize.for` (15m).
- **The container memory limit went from 256Mi to 384Mi**, and `GOMEMLIMIT`
  now follows the limit. The LWIN dictionary uses about 55 MiB resident and
  peaks around 180 MiB while it is being built. Check any values override that
  pins `resources.limits.memory`.
- **LWIN stamping is now a per-source opt-in** (nagus-a8t). A source writes
  canonical ids only when the global `lwin.stamp` / `NAGUS_LWIN_STAMP` switch is
  on AND the source sets `lwinStamp: true`. If a source was stamping before, it
  stays shadow-only until you add `lwinStamp: true` to it.
- **`/watches` now returns 200 with a per-watch `error` field** where it used to
  return a 500 for the whole response. A consumer that relied on a non-200
  status to detect a broken watch now has to read `error` on each watch.
- **Offer store schema: additive migration on first start.** Offers gain
  resolution columns (Postgres uses `ADD COLUMN IF NOT EXISTS`; SQLite checks
  `table_info`). The `provisional_key` column is kept but no longer written, and
  its index is dropped with `DROP INDEX IF EXISTS`. No destructive DDL runs.
- **Items that a category now rejects are deleted on the next ingest.** This
  covers SSDs in `hdd`, and merchandise or listings with no wine evidence in
  `wine`. Expect the item count to drop once after upgrade.

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

- **quark product-identity resolution** (nagus-6r6, quark QUARK-07; chart
  0.6.0). nagus asks quark which product each offer names and stamps the
  answer on the offer. This happens as asynchronous enrichment in its own
  goroutine, so a slow, down or misconfigured quark can never fail an ingest or
  a surface.
  - Offers gain `Resolution{State, ProductID, Generation, At}` with the states
    `unattempted | resolved | refused | quarantined | unidentifiable`.
    Ingest's upsert never erases a resolution. The stored answer is kept while
    the product hint is unchanged and reset when the hint changes, all in one
    atomic `ON CONFLICT` statement. An answer is stamped only against the
    fingerprint of the hint that was sent, so if a listing changes while the
    call is in flight, the stale answer is discarded.
  - A refusal is re-offered only after quark's catalog generation has moved
    past the generation it was refused under. Never-asked offers are sent
    first, so a generation bump cannot starve them.
  - **An offer with an entirely empty hint is `unidentifiable`, recorded
    locally and never sent.** Measured against the real quark image, 32 of 54
    offers were hint-less. Sending them would have held quark's
    `refused_ratio` alert at 0.59, so it would have fired permanently.
  - Env: `NAGUS_QUARK_URL`, `NAGUS_QUARK_INTERVAL` (default 10m),
    `NAGUS_QUARK_TOKEN`, `NAGUS_QUARK_BATCH_SIZE` and `NAGUS_QUARK_TIMEOUT`. The
    pass is OFF, with the reason logged, when the URL, the token or the offer
    layer is missing. Chart: `quark.url`, `quark.interval` and
    `quark.tokenExternalSecret`. The token ref is `optional: true` because with
    `strategy: Recreate` a required ref to an unsynced Secret would take down
    every surface.
- **Product ids on rows.** Rows from `/search`, `/watches` and MCP
  `search_items` carry quark's `product_id` when the offer is resolved. Two
  rows with the same id are one product sold by different sellers. If the
  lookup fails, the field is left empty and the read still succeeds.
- **Metrics and alerts** (nagus-ff4, chart 0.7.0). New ServiceMonitor and
  PrometheusRule with the alerts `NagusDown`, `NagusQuarkUnauthorized` (any 401
  in 15m, which is always a token problem) and `NagusQuarkEnrichmentStalled`.
  The stall alert keys on `nagus_quark_last_clean_pass_timestamp_seconds`
  rather than on error counters: an idle nagus and a broken one both stop
  moving counters, but only the broken one stops finishing clean passes.
  `/metrics` also gains `nagus_quark_*` pass counters and
  `nagus_lwin_{records,loaded_timestamp_seconds,refresh_failures_total,stamping}`.
- **LWIN is mirrored from the published Liv-ex export** (nagus-93q, chart
  0.8.0). Earlier, nagus waited for someone to supply a CSV by hand. Details:
  - `lwin.LoadXLSX` streams the workbook with no new dependency. Only Live
    Wine/Fortified Wine rows load: 185,383 records, about 6s to build.
  - `internal/refdata` handles the mirror: a conditional ETag download, a size
    cap, and validation before it replaces the file. A failed refresh keeps the
    last good copy. It is written so quark can reuse it.
  - One dictionary is shared per process and swapped atomically by a daily
    refresh. It loads in the background, so startup and hdd never wait on
    Liv-ex. A wine source's FIRST ingest waits for it (bounded at 10m;
    nagus-0k0); without that wait, each restart cost 12h of identity data.
  - Chart keys: `lwin.url`, `lwin.cachePath`, `lwin.maxAge` (720h) and
    `lwin.stamp` (default false, which is shadow mode). Shadow items record
    `lwin_route`, `lwin_candidate` and `lwin_score`.
- **LWIN matching uses the store's producer** (nagus-86s). Producer-storefront
  titles never name the producer, so title-only matching was mostly wrong: 48
  of 49 auto-matches on live listings. Now:
  - A wine source may declare `wineProducer`. Otherwise, a producer-channel
    source uses the product vendor. A retailer's vendor is never trusted.
  - The producer is a HARD constraint on candidates.
  - Auto-routing needs 80% coverage of the title's distinctive tokens, and
    geography (SUB_REGION, SITE) breaks ties.
  - An uncovered title word that names a sibling wine from the same producer
    blocks auto-routing and sends the match to adjudication.

  Result on Robert Mondavi: 24 auto matches, 0 wrong.
- **Retailer producer hints and bracketed critic codes.** `producerFromBody`
  is a per-source opt-in that reads "Producer: X Region: Y" from a structured
  description. Without it, a retailer listing whose vendor field is literally
  "WINE" carries no producer. The critic parser also accepts
  `[JS98][WA97][WS96]` and `[V90]`. WA and V are recognized only inside
  brackets, since bare "WA" means Washington state. Rows carry
  `ship_legal_to`, because a row surfaced for a gift or another household
  member has to say where the bottle can legally ship. Comparables and critic
  scores are left to quark (see
  `docs/design/2026-09-23-wine-market-signals-boundary.md`).
- **Wine connectors: Commerce7, Vinoshipper, OrderPort and Dynamics 365
  Commerce** (nagus-b0u, nagus-tub, nagus-oc4, nagus-29b, nagus-390). Each
  reads a public storefront endpoint and paces requests politely:
  - OrderPort falls back to the store's own listing link.
  - Dynamics 365 decodes the embedded list state and honors the site's 10s
    Crawl-Delay.
  - Multi-bottle bundles and sets are skipped.
  - When the source provides structured vintage, varietal and bottle size,
    those fields feed the extractor. A seller's ships-to list can only narrow
    `ship_legal_to`, never widen it.

  A shared `webfetch` client sends a descriptive User-Agent and caps
  `Retry-After`.
- **`nagus fingerprint DOMAIN...`** detects a store's commerce platform and
  the endpoint a connector would read. It honors robots.txt.
- **Store-sale deal signal.** The Shopify connector carries
  `compare_at_price`, and the wine extractor derives `list_price_cents` and
  `discount_pct`. A watch's `min_discount_pct` marks a store sale as strong.
  This is a SALE signal (the seller cut the price), not a value verdict, and
  the delivery message says so.
- **New-release signal from Shopify `published_at`** (nagus-cux). A watch's
  `new_within_days` marks anything the store published within that window as
  strong, whatever the price. Setting only `new_within_days` does not also
  apply the default great-verdict rule.
- **Per-category row details.** Rows gain `category` and a whitelisted
  `details` map covering vintage, varietal, colour, bottle size, score,
  discount, list price, `published_at`, producer and land fields. Delivery can
  then format each category properly instead of printing every listing as a
  hard drive.
- **TTB COLA label approvals as a `release` category** (nagus-0ek).
  Allocation-only producers have no store to watch, but every label needs a
  TTB Certificate of Label Approval first. The `ttbcola` connector searches the
  registry per brand over a lookback window, pausing 5s between requests.
  - Config: `sources[].colaBrands`, `colaLookbackDays` (default 45) and
    `categories.release.releaseFreshDays`.
  - An approval stays `new-label` for 30 days, so a watch can ping on
    `strong_verdicts: ["new-label"]`.
  - ttbonline.gov omits its intermediate certificate. nagus embeds the missing
    intermediate and still verifies the full chain, hostname and key usage
    against the system roots.
- **IMAP deals-mailbox source** (nagus-239, nagus-zsi). Some sellers publish
  offers only by email. A source declares exactly one sender (`imapFrom`), plus
  optionally `imapParser`, `imapDkimDomain`, `imapMailbox`, `imapLookbackDays`
  and `imapForwarders`. Server env: `NAGUS_IMAP_HOST`, `NAGUS_IMAP_PORT`,
  `NAGUS_IMAP_TLS`, `NAGUS_IMAP_USERNAME` and `NAGUS_IMAP_PASSWORD` -- the
  chart does not render these; supply them from a Secret loaded with
  `envFrom` (the source ExternalSecret).
  - The mailbox is read-only (`EXAMINE`) and stateless: messages are keyed by
    Message-ID. Attachments are never read, and oversized messages are skipped.
  - `imapForwarders` lists household addresses in config only. A forward is
    accepted when it is DKIM-verified for the forwarder's own domain and its
    forwarded original names the source's sender. Such items are tagged
    `mail_forwarded_by`.
  - Each sender needs a Parser written from a real captured email. None ship
    yet, so an imap source fails at startup and says why.

### Changed

- **quark product ids replace the local provisional key.** Deleted:
  `ComputeProvisionalKey`, `Offer.ProvisionalKey` and `Query.ProvisionalKey`.
  This meets spec D4 (no permanent local fallback). Production held 252 offers
  resolved to 234 quark ids with 0 unattempted. Grouping now uses
  `product_id`. A frozen, test-local copy of the old algorithm remains only for
  the serverpartdeals parity baseline.
- **quark batches: 100 hints per call, 2m timeout** (previously 500 and 30s).
  quark served about 270ms per hint on orac, so every call over roughly 110
  hints timed out. quark had already applied the work, nagus discarded the
  answer, and the backlog never drained. A test pins the default batch at the
  measured latency to 2x headroom inside the timeout.
- **Wine extraction prefers the title over stale structured fields.** Stores
  reuse product records across releases, for example a 2022 title carrying a
  2021 vintage. The precedence is now title, then structured field, then
  description. The varietal table gains grapes seen on live stores, such as
  Mencia, Semillon, Roussanne and Gamay.
- **Category rules keep SSDs out of `hdd` and merchandise out of `wine`**
  (nagus-17k). On live data, 59 of 421 hdd rows were SSDs, and 7 of them ranked
  'great' against spinning-disk references. A wine item with no vintage,
  varietal or colour is also rejected: all 12 such items in the live corpus
  were merchandise. SSHD hybrids stay in `hdd`.

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

- **One bad watch no longer silences every watch.** `EvaluateAll` used to abort
  on the first failing watch, which turned `/watches` into a 500, and the
  delivery cron then pinged nothing. Each watch now carries its own fixed
  `error` text (watch name and category, never a raw store error).
- **Items their category now rejects are removed.** Shopify sources have no
  freshness purge, so SSDs and merchandise stored before the rules existed
  would otherwise have stayed forever. Item stores gain `Delete(id)` on all
  three adapters, and an extract error wrapping `listing.ErrNotInCategory`
  deletes the previously stored item. Ordinary extract errors do not.
- **The glovebox gate trips on a rejected token** (nagus-lg7, chart 0.10.0).
  Previously nagus retried glovebox for every listing after the first 401,
  which tripped glovebox's brute-force limiter, and nothing paged. Now the
  first 401 or 403 opens a 5m cooldown: listings drop without a call, one log
  line names the Vault fields to compare, and a single probe follows.
- **The glovebox token is a re-read file** (nagus-4g3). The pod started before
  its ExternalSecret synced, read an empty env var once, and dropped everything
  until someone restarted it by hand. The token is now an optional mounted
  Secret file (`NAGUS_GLOVEBOX_TOKEN_FILE`). It is re-read whenever the gate has
  no token and after every trip, so a late sync or a rotation heals without a
  restart. `NAGUS_GLOVEBOX_TOKEN` still works.
- **IMAP DKIM is verified by nagus itself.** The deals MX writes only ARC sets,
  no Authentication-Results header, so an A-R-only check would have silently
  rejected every message. See Security.
- **CI: main was red for six weeks because of an image reference, not the
  code.** The gate image `golang:1.26.6` is a fat index that the in-cluster zot
  mirror cannot sync, so every job pod died as `runner_system_failure` and
  no image was published from 2026-08-02 on. The gate now pins the same
  single-platform digest as the Dockerfile's amd64 build leg, so the gate and
  the shipped binary use one toolchain string. The CI postgres service is
  pinned by digest too, keeping its `postgres` alias.
- **CI: vulncheck download failures are no longer findings** (nagus-1gb).
  govulncheck is installed with retries and backoff. Exit 3 (vulnerabilities
  found) fails at once; any other non-zero exit is retried up to three times.
- **CI: Postgres test isolation** (nagus-0wj). Each test package gets its own
  database, created from `template0` with retry on error 55006. Each package
  builds its store once and isolates cases with `DELETE` instead of `TRUNCATE`,
  and the test waits up to 90s for the service. A setup timeout logs
  `pg_blocking_pids`.
- **Reproducible serverpartdeals dedup baseline.** A committed capture
  (`testdata/serverpartdeals_2026-09-12.json`) yields 104 offers and 35
  products. The emitted hints fixture is copied verbatim into quark's parity
  check. The earlier "250 -> 132" figure came from a capture that was never
  committed.

### Security

- **glovebox sanitize gate for every source, fail closed** (nagus-9ib; chart
  0.9.0). Passthrough was honest only while no listing text reached an LLM.
  Watch rows now reach an agent, and a mailbox is an open channel. The gate
  classifies title, body and aspects, and only `verdict=pass` keeps an item,
  with its original bytes. Quarantine, errors and transport failures all drop
  the item. 429 and 503 are retried first.
  - The client is generated from glovebox's own OpenAPI spec.
  - Env: `NAGUS_GLOVEBOX_SANITIZE_URL` plus `NAGUS_GLOVEBOX_TOKEN` or
    `NAGUS_GLOVEBOX_TOKEN_FILE`. Chart: `glovebox.sanitizeURL` and
    `glovebox.tokenExternalSecret`.
  - Half-configured (URL without a token): nagus starts, serves what it holds,
    and drops every NEW listing. It never runs ungated and never crashloops.
  - Chart 0.10.0 adds `nagus_sanitize_total{outcome}` and the alerts
    `NagusSanitizeUnauthorized`, `NagusSanitizeNoToken` and
    `NagusSanitizeDroppingEverything`. The last is critical: more than
    `alerts.sanitize.dropRatio` of at least `alerts.sanitize.minListings`
    listings dropped over 30m.
- **The mailbox trusts only verified senders.** A From header is trivially
  forged, so a message is accepted only when one of these holds:
  - nagus verifies a DKIM signature aligned to the sender's domain (via
    go-msgauth; `l=` and rsa-sha1 are refused, and From must be signed);
  - the TOPMOST Authentication-Results header comes from a trusted authserv-id
    and reports `dkim=pass`.

  Deeper A-R headers and unverified ARC seals are sender-writable and are
  never consulted. Tests cover forged deeper A-R headers, authserv suffix
  tricks, forged ARC and tampered messages. `golang.org/x/crypto` went to
  v0.57.0.
- **MCP: no listing values in the text block; fixed internal errors; strict
  arguments** (nagus-w1p).
  - `search_items` and `get_item` results, including seller-authored titles,
    now travel only in `structuredContent`. The text block, which a client may
    put straight into model context, carries a count and a pointer, and it no
    longer echoes a missing id.
  - Internal failures return a fixed message and log the detail to stderr
    instead of returning `err.Error()`, which could carry a DSN fragment or a
    path.
  - Arguments are decoded with `DisallowUnknownFields`, so the server enforces
    `additionalProperties: false`.
  - The openclaw bridge already renders `structuredContent`, so household
    agents see the same data as before. For them, glovebox remains the
    injection defence.

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
