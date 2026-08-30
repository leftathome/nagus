# Design spec: wine category on a $0/mo data stack (+ multi-category roadmap)

- **Status:** accepted 2026-08-30 (revises an earlier report that recommended
  the paid Wine-Searcher API)
- **Implements:** the wine category bundle (`internal/category/wine.go`), the
  LWIN identity resolver (`internal/identity/lwin`), critic-score
  normalization + hedonic value (`internal/valuation/wine`), the wine
  extractor (`internal/extract/wine`), and the WA ship-legality model.
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
| Offers | WA retailers / winery-direct / flash sites on **Shopify** (`products.json`), Total Wine + Wine.com JSON-LD, Wine.com Rakuten feed | free; per-site verification required | The existing shopify connector already covers Shopify storefronts. JSON-LD and IMAP digest connectors are follow-ons (JSON-LD parsing likely belongs in glovebox -- connectors are its lane). |

**Vivino is a NO-GO** (no API, anti-scraping posture, affiliate program
exposes no ratings). Paywalled critic sites (Wine Spectator app, Suckling,
Vinous, Decanter) are not fetched; their scores reach us legitimately via
retailer attributions and GWS aggregation.

### 1.3 Shipping legality is a CONSTRAINT LAYER, not a hardcoded WA rule

US direct-to-consumer wine shipping law is per-destination-state and
per-channel, and the household's destination is not always Washington --
buying a gift for someone in CA or FL is a first-class use. So legality
lives in `internal/shipping` as a data-driven rules table, with WA merely
the motivating worked example (out-of-state retailers may not ship into WA;
SB 5007, which would have permitted them, died in committee Jan 2024;
permitted wineries and in-state retailers may, per WSLCB / RCW 66.20).

The model, all implemented:

- A source DECLARES how and from where it ships: `wineChannel`
  (`winery_direct` | `retailer`) + `state` (USPS code) in config. Both are
  required -- legality is a conscious per-source declaration, never a
  default, and a missing/unknown declaration is a startup error.
- `shipping.Rules` maps every destination state (50 + DC) to a `Policy`
  {wineryDirect, inStateRetailer, outOfStateRetailer}. `DefaultRules()` is
  a documented engineering baseline (winery-direct legal everywhere except
  MS/UT; in-state retailer delivery legal everywhere; out-of-state retailer
  legal into a short list -- NOT legal advice: verify a destination before
  relying on it). An operator overrides any destination from a JSON file
  (`NAGUS_WINE_SHIP_RULES`, merged over the defaults), because these laws
  churn with legislation and litigation.
- The channel tagger stamps each listing with the source's declaration and
  its computed legal-destination SET (`ship_legal_to`, a token set of state
  codes, tokens validated at extract). Stamping the whole set -- not one
  state's boolean -- means one ingested corpus serves watches for ANY
  destination; a rules change converges on the next poll's re-stamp.
- A surface/watch picks its destination: `wineShipTo` in category config
  (env `NAGUS_WINE_SHIP_TO`). The filter then hard-requires the destination
  token via the generic `score.Filter.HasToken` predicate, and the gate
  FAILS CLOSED at every layer: unknown destination, unknown channel,
  unstamped item, empty legal set -- all illegal, never default-legal.
  Empty `wineShipTo` = no legality filter (all offers informational).
- Offers that cannot reach the configured destination still ingest
  (price signal for the corpus) but never surface as actionable.
- Re-verify a source's declaration when onboarding it (a flash site's
  fulfillment model can change); the config declaration records that
  verification.

## 2. What this slice implements (and the algorithms)

```
shopify/other connector
  -> TagWineChannel (stamps wine_channel + source_state + the computed
       ship_legal_to destination set from the shipping rules table)
  -> sanitize (boundary marker; production path = glovebox)
  -> extract/wine: vintage, bottle_ml, varietal, colour,
       critic attributions -> normalized wine_score + wine_score_count,
       LWIN resolution -> CanonicalID (auto-route only)
  -> store
  -> hard-filter: priced, budget, min wine_score,
       ship_legal_to contains the configured destination (wineShipTo)
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

**Value model**: `log(price_cents) = a + b*(s-80) + c*(s-80)^2 +
d*1[s>=90]`, because the superstar premium is real and non-linear -- the JWE
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
