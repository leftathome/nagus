# Design spec: wine category on a $0/mo data stack (+ multi-category roadmap)

- **Status:** accepted 2026-08-30 (revises an earlier report that recommended
  the paid Wine-Searcher API)
- **Implements:** the wine category bundle (`internal/category/wine.go`), the
  LWIN identity resolver (`internal/identity/lwin`), critic-score
  normalization + hedonic value (`internal/valuation/wine`), the wine
  extractor (`internal/extract/wine`), and the per-jurisdiction
  ship-legality constraint layer (`internal/shipping`).
- **Parent:** `2026-07-01-nagus-design.md` (the spine, category = plugin
  bundle). Read that first; this document only fills the wine bundle and
  records the source decisions.

## 1. Decision log (the crux items)

### 1.1 Wine-Searcher: REJECTED, in both paid and "free trial" form

The $250/month API is rejected on cost. The "100 free calls/day" tier is
rejected too: their trade page's own language ("Start out with a **trial** of
100 free calls per day -- **upgrade when ready**") marks it an onboarding
trial, not a documented perpetual free tier. Building a long-lived automated
watcher on it risks silent revocation and a plausible ToS violation for
indefinite non-paying automated use -- and the API is functionally thin
anyway (first-24-merchants, single-product name per call, quota reset
midnight UK). **No automated path may depend on Wine-Searcher.** A human
wanting a one-off aggregated look-up uses the website by hand.

### 1.2 The free stack that replaces it

| Role | Source | Access | Notes |
|---|---|---|---|
| Identity | **LWIN** (Liv-ex) | Creative Commons, free forever; form download, refresh ~quarterly | THE canonical id: LWIN-7 producer/wine, LWIN-11 +vintage, LWIN-16 +size. Lowest-risk source in the stack. Loaded via `NAGUS_LWIN_CSV`. |
| Quality (aggregated) | **Global Wine Score API** | free token tier, 10 req/min, attribution | LWIN-keyed; CDF-normalized unweighted mean, min 3 ratings. Public OpenAPI mirror dated 2020 -- CONFIRM token issuance at signup before building the connector. |
| Quality (crowd + critics) | **CellarTracker** `xlquery.asp` export | free account, documented personal export | Poll daily at most; use the maintained `cellartracker` wrapper semantics (User/Password/Format/Table params). |
| Quality (per-listing) | **Retailer-published critic scores** ("WS 92", "JS 94") | parsed from the offer fetch itself | Implemented in this slice: deterministic regex first; the local-LLM structuring fallback is a follow-on. |
| Cold-start value prior | Kaggle Wine Enthusiast 130k reviews | free, static (~2017) | Fits the hedonic prior offline; the UCI "Wine Quality" physicochemical set is NOT a quality signal -- do not use it. |
| Offers | Retailers / winery-direct / flash sites on **Shopify** (`products.json`), worldwide, Total Wine + Wine.com JSON-LD, Wine.com Rakuten feed | free; per-site verification required | The existing shopify connector already covers Shopify storefronts. JSON-LD and IMAP digest connectors are follow-ons (JSON-LD parsing likely belongs in glovebox -- connectors are its lane). |

**Vivino is a NO-GO** (no API, anti-scraping posture, affiliate program
exposes no ratings). Paywalled critic sites (Wine Spectator app, Suckling,
Vinous, Decanter) are not fetched; their scores reach us legitimately via
retailer attributions and GWS aggregation.

### 1.3 Shipping legality is a CONSTRAINT LAYER, not a US rule

Direct-to-consumer wine law is per-destination and per-channel, and the
destination is not always Washington -- or even the US. A household buys for
itself, and buys gifts for people in Barcelona, Toronto, or Melbourne. So
legality lives in `internal/shipping` as a data-driven rules table over
JURISDICTIONS, with the US case as one region of it rather than the schema.

**The model.** A jurisdiction is an ISO 3166 code: a country (`FR`, `AU`)
optionally with a subdivision (`US-WA`, `CA-ON`). Both a source's origin and a
watch's destination are jurisdictions. Law turns on two things, and the table
is indexed by exactly those:

- the **channel**: `producer` (the winery shipping its own wine) or
  `retailer` (a licensed reseller). Nearly every market treats these
  differently.
- the **origin relation**: same subdivision (an in-state/in-province seller),
  same country (interstate/interprovincial), same trade bloc (the EU single
  market's excise distance-selling regime), or foreign (a third country).

Each destination therefore carries a `Policy`: per channel, which relations
may ship to it. That expresses the cases that actually differ -- a WA retailer
may ship within Washington while a California retailer may not ship in (SB
5007 died in committee, Jan 2024); a French winery may distance-sell to a
Spanish consumer but not to a US one, because US imports must clear a licensed
importer; a BC winery reaches Manitoba but not Ontario.

**The baseline table** (`DefaultRules`) covers ~110 destinations: the US per
state, Canada per province, the EU-27 at country level with the single market
as a bloc, and other major wine markets (GB, CH, AU, NZ, AR, BR, CL, MX, UY,
ZA, JP) at country level. Confidence is documented per region in
`internal/shipping/defaults.go`. It is a good-faith engineering baseline, NOT
legal advice.

**Everything fails closed**, at every layer: an unknown or malformed
jurisdiction, an unmodeled destination, an unknown channel, an unstamped item,
or an empty legal set is illegal -- never default-legal. Two deliberate
consequences:

- The table OMITS destinations whose regime we could not state (Norway,
  Iceland, several Asian markets) rather than encoding an all-false entry that
  would look modeled. "Unmodeled" and "prohibited" are different facts, and
  `Rules.Modeled` distinguishes them: configuring an unmodeled destination is
  a startup error naming the override path, not a silently dark surface.
- The `foreign` dimension is off almost everywhere by default. Several markets
  (AU, NZ, GB, JP) do permit personal importation subject to duty; that is a
  one-line override, deliberately not a default.

**Mechanics.** A source declares `wineChannel` + `origin`; both required, and
a missing or malformed declaration is a startup error. The channel tagger
stamps each listing with the declaration and its computed legal-destination
SET (`ship_legal_to`, e.g. `US-WA US-CA FR`), tokens validated as ISO 3166 at
extract. Stamping the whole set -- not one destination's boolean -- means one
ingested corpus serves watches for ANY destination, and a rules change
converges on the next poll's re-stamp. A surface picks `wineShipTo` and the
generic `score.Filter.HasToken` predicate enforces it.

### 1.4 Currency: an international corpus needs FX or an honest "unknown"

Going international introduces a trap this codebase has hit before in other
forms -- the failure looks like success. The hedonic model is fit in one
currency (USD by default); a EUR listing whose price is compared against it
directly would be mispriced by whatever the exchange rate is, and would emit a
confident verdict rather than an error.

So `HedonicModel` declares its `Currency`, and the `Valuer` takes optional
`Rates` (operator config, not a live FX call: a stale rate nudges a verdict,
whereas a live dependency on the read path could fail or hang a surface, and
wine prices do not move on intraday FX). A listing in an unrated foreign
currency yields `unknown-no-reference` -- unplaceable, never mispriced. An
EMPTY currency reads as the model's own, because a connector omitting the
field is a data gap rather than evidence of a foreign price, and darkening
every such item would be the worse error.

## 2. What this slice implements (and the algorithms)

```
shopify/other connector
  -> TagWineChannel (stamps wine_channel + source_origin + the computed
       ship_legal_to jurisdiction set from the shipping rules table)
  -> sanitize (boundary marker; production path = glovebox)
  -> extract/wine: vintage, bottle_ml, varietal, colour,
       critic attributions -> normalized wine_score + wine_score_count,
       LWIN resolution -> CanonicalID (auto-route only)
  -> store
  -> hard-filter: priced, budget, min wine_score,
       ship_legal_to contains the configured destination jurisdiction
       (wineShipTo: US-WA | FR | CA-BC | ...)
  -> valuation/wine: hedonic log-price residual -> verdict
  -> score -> rank -> surface / watches
```

**Critic-score normalization** (`internal/valuation/wine`): 100-point scores
pass through (plausibility-bounded 50..100); 20-point scores (Jancis
Robinson et al.) map through piecewise-linear anchors (12->76 per
Cardebat-Paroissien, 20->100; deliberately NOT linear 5x), with an optional
per-critic bias table standing in for full CDF standardization until
historical distributions are wired. Aggregation dedupes per critic (highest
wins -- retailers repeat attributions) and takes the unweighted mean.
Inter-critic agreement is only r ~ 0.34-0.60 (JWE: Ashton 2012/2013, Luxen
2018, Masset et al. 2015), so the GWS-style **minimum-3 rule** applies:
value is never flagged on fewer than 3 independent scores unless the
operator explicitly lowers `minWineScoreCount`.

The "WA" shorthand for The Wine Advocate is deliberately not recognized (in
this home market "WA" is Washington and sits next to numbers constantly);
The Wine Advocate parses as "RP" or by full name.

**Value model** (in the model's own currency; see 1.4):
`log(price_cents) = a + b*(s-80) + c*(s-80)^2 + d*1[s>=90]`, because the superstar premium is real and non-linear -- the JWE
superstar study (266k Wine Spectator reviews) found the price premium
significant only above 90 points, and Ali/Lecocq/Visser (2008) found the
critic effect absent for low scores. A naive points-per-dollar ratio is
therefore wrong by construction. The default coefficients are documented
bootstrap priors approximating the Kaggle 130k distribution; verdicts come
from the log-price residual in units of the fit's residual spread (great
<= -1.5z, good <= -0.5z, market <= +0.5z, else poor). Prices are scaled to
750ml-equivalent first so magnums are not flagged expensive. Refit offline
on live offers as they accrue and inject via `WineDeps.Model`.

**LWIN entity resolution** (`internal/identity/lwin`): normalize (accent
fold, Chateau/Domaine alias expansion), block on shared tokens, score with
token-set ratio (Jaro-Winkler tiebreak), route by confidence: >=92
auto-stamp LWIN-11 onto `CanonicalID`; 80-92 `lwin_route=adjudicate` (the
future local-LLM tier picks among top-5 candidates); <80 review. **Only
auto-route matches ever stamp an identity** -- a wrong canonical id silently
corrupts every downstream quality join, which is worse than no id (target:
<5% false auto-match on a labeled sample before trusting the threshold).

## 3. Follow-on work (in build order)

1. **GWS connector** (quality enrichment keyed by LWIN; 10 req/min budget,
   `?ordering=-date` incremental sync). Gate: confirm the free token tier
   still exists at signup; fall back to CellarTracker + retailer scores as
   the quality spine if not.
2. **CellarTracker export connector** (creds via Vault/ExternalSecret, daily
   poll, low volume).
3. **Retailer JSON-LD fetch** for Total Wine / Wine.com product pages (WA
   store/ship-to context; respect robots; low volume) -- likely a glovebox
   connector, filed against that repo.
4. **IMAP digest parsing** for flash/daily-deal offers (Last Bottle, WTSO --
   time-boxed offers are push-driven; email is the pragmatic event source).
5. **LLM tiers**: adjudicate-band LWIN resolution; free-text critic
   attribution structuring the regexes miss. Both run on sanitized text and
   emit typed labels only (design section 7 containment).
6. **Hedonic refit** pipeline: fit offline on Kaggle + accrued live offers,
   ship coefficients as config.

## 4. Multi-category roadmap

nagus's category-=-bundle abstraction (design section 5) IS the revised
spec's `CategoryAdapter`: `resolve_identity` = the extractor's canonical-id
stage, `offers` = connectors, `quality`/`value_model` = the valuation
adapter behind the bundle's Valuate hook. No new plugin interface is needed;
wine is the third fill after land and HDD, and it required exactly one
generic-spine change (the `EqAttr` filter predicate) -- the abstraction is
holding.

Next categories, ranked by free-stack readiness (canonical id + free
machine-readable quality + free offers):

1. **Video games** -- IsThereAnyDeal API v2 (free key, 1000 req/5min
   verified) for offers, OpenCritic via RapidAPI (free: 200 req/day, 25
   searches/day -- cache hard) for quality, ITAD/Steam AppID identity.
2. **Board games** -- BoardGameGeek XML API2 (free; ~2 req/s; API key now
   required for most endpoints) gives ratings AND a marketplace; BGG id.
   If the key gate stalls, slot physical media in instead.
3. **Physical media** -- TMDB + OMDb (free keys) quality, eBay (already
   built) offers, IMDb/TMDB id.

Then: Magic cards (Scryfall: free, bulk files, UA+Accept mandatory), books
(Open Library / Google Books; Goodreads API is dead), LEGO (BrickLink
approval gate ~1-2 weeks). Skip: music gear (no quality axis), coffee/
spirits (scrape-only), consumer electronics (no free quality source).

## 5. ToS / risk notes

- LWIN: CC-licensed, attribution -- lowest risk; keep the attribution in
  any surfaced output that exposes LWIN data.
- GWS: attribution required on the free tier; 10 req/min hard budget
  (cron-pace >= 7s).
- CellarTracker: documented personal export; keep volume low (daily).
- Shopify `products.json`: public storefront feature, but storefronts rate
  limit -- polite intervals (the shopify connector already backs off).
- Total Wine / Wine.com page fetches: grey; respect robots.txt, low volume,
  prefer the sanctioned Rakuten feed for Wine.com.
- Wine-Searcher / Vivino: prohibited in automated paths (1.1, 1.2).
- Craigslist remains PROHIBITED entirely (CLAUDE.md hard rule; unrelated to
  wine but restated because "flash deal sources" must never grow one).
