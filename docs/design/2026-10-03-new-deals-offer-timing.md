# New deals since T: offer timing on agent rows, expired hiding, `new_deals` and `list_watches` (2026-10-03)

Bead: nagus-417. Related: nagus-1xy (agent surface too thin), nagus-q6u (the
offer layer), nagus-m8f (restock signal, deferred). Operator decisions of
2026-10-03 are marked DECIDED; everything else is this build's choice and the
reason is given.

## Why

Caspar (openclaw) should answer "show me new good deals on things I care
about". Before this change it could not:

- MCP and HTTP rows carried no timing at all. The one timestamp an item has,
  `items.seen_at`, is stamped by every connector on every fetch and
  overwritten by the upsert: it is LAST POLLED, not first seen (its comment
  said otherwise).
- Expired listings still surfaced. Offers expire (not seen for 3 polls), but
  items are purged only for eBay (6h) and deal submissions (>=24h); every
  other source keeps items forever and the read path never consulted the
  offer.
- Watches ("things I care about") were HTTP-only.

## DECIDED

1. "New since" is STATELESS: the caller passes a timestamp (the time it last
   asked); nagus keeps no per-caller cursor.
2. HIDE EXPIRED listings from `search_items`, `/search` and watch results.
   `get_item` (an explicit lookup) still answers, marked expired.

## A. Timing fields on rows (additive)

Every agent-facing row -- `search_items`, `/search`, `/watches` candidates and
strong, `new_deals`, `list_watches` matches, `get_item`, `/item` -- gains these
fields, taken from the listing's OFFER (item id == offer id):

| field | meaning |
|---|---|
| `first_seen_at` | RFC 3339 UTC. When nagus first saw this listing at this source (`offers.first_seen_ns`; never resets, a revived offer keeps it). |
| `last_seen_at` | The most recent poll that saw it (`offers.last_seen_ns`). |
| `price_changed_at` | When the price last changed to a different non-zero value; equals `first_seen_at` until it does. |
| `previous_price_cents` | The price before that change; absent when the price never changed or the earlier price was unknown. |
| `lowest_price_cents` | Lowest non-zero price ever seen for this listing (`offers.min_price_cents`). Undated. |
| `at_lowest_price` | `price_cents` equals `lowest_price_cents`. |
| `offer_status` | `active` or `expired`. |
| `expired_at` | Only when expired: when nagus marked it expired. |
| `expires_at` + `expires_basis` | See below. |
| `offer_note` | Only on an expired listing (get_item, /item): a fixed sentence saying it must not be recommended. |

`expires_basis` says how much `expires_at` is worth, so an assumption is never
presented as a fact:

- `listing_end`: the SOURCE states the end (an eBay auction's `itemEndDate`).
- `email_window`: a deal submission. The email is read for the mailbox's
  lookback window (14 days by default), so the deal stays listed until the
  mail's arrival plus that window. That is nagus's own retention, not a
  statement by the seller that the deal ends then.
- `not_seen_3_polls`: an ASSUMPTION. `last_seen_at + 3 x poll interval` is
  when nagus will mark the offer expired if no poll sees it again. Nothing
  says the seller will stop selling it then; a listing that keeps appearing
  never reaches it.

When the offer layer is off, or a row has no offer (the `release` category
has none by design; a listing ingested before the offer layer was enabled),
every one of these fields is ABSENT -- never a zero value. The tool
descriptions say so.

`items.seen_at` keeps its name and value for compatibility; its comment now
says it is the last poll, and `get_item` / `/item` add the fields above
alongside it.

## B. Storage

Three columns on `offers`, in Postgres, SQLite and memory:

- `price_changed_at_ns BIGINT NOT NULL DEFAULT 0`
- `previous_price_cents BIGINT NOT NULL DEFAULT 0` -- needed to tell a price
  DROP from a rise without a price-history table (one value, not a series).
- `ends_at_ns BIGINT NOT NULL DEFAULT 0` -- a source-stated end; 0 = none.

The rule, in the same upsert statement (Postgres: CASE expressions over the
stored row; SQLite: the existing read-then-write transaction, which now also
reads the stored price; memory: the same fold):

| stored price | incoming price | price_changed_at | previous_price_cents |
|---|---|---|---|
| (first insert) | any | first_seen | 0 |
| P | P (same) | unchanged | unchanged |
| P | Q > 0, Q != P | the incoming observation time (never moves backwards) | P |
| P | 0 (unknown) | unchanged; `price_cents` KEEPS P | unchanged |
| 0 (never known) | Q > 0 | unchanged (the first known price is not a change) | 0 |

The offer's stored `price_cents` is the last KNOWN price: a poll reporting 0
(unknown) no longer erases it (review of !37, L1), so 1000 -> 0 -> 900 is a
drop from 1000 and 1000 -> 0 -> 1000 is no change. Rows show the ITEM's
price as before; the offer price feeds only the comparisons. By design, a
seller going to "no price" therefore leaves the offer at its last known
price, and `at_lowest_price` compares THAT price with the lowest seen.

A drop after a rise (800 -> 1000 -> 900) is a `price_drop` (lower than the
previous price); `at_lowest_price` false says it is not a new low.

`ends_at_ns` is overwritten by every poll with what the source states now
(0 when it states nothing), so a listing that loses its end time loses it here
too.

### Migration and its cost

Postgres (the production backend): `ALTER TABLE offers ADD COLUMN IF NOT
EXISTS ... NOT NULL DEFAULT 0` for each column, in the schema `New` runs on
every startup. Since PostgreSQL 11 adding a column with a constant default is
a catalog-only change: no table rewrite, an ACCESS EXCLUSIVE lock held for
milliseconds, cost independent of row count.

The backfill (`price_changed_at_ns = first_seen_ns` where it is 0) is NOT run
by `New`. The first version did, as one statement, and measuring it on
1,000,000 rows (below) showed 39.5 s with the heap doubled until vacuum --
longer than the pod's liveness budget (5 s + 3 x 10 s), so a large table
would crash-loop the Recreate deployment. It is `offer.Backfiller`, which
`nagus serve` runs in a goroutine after startup: primary-key batches of 5,000
(`id > last ORDER BY id`), each its own short transaction, so an ingest upsert
of a row in a running batch waits for that batch only. A later startup finds
nothing to do from one EXISTS probe.

The read path does not depend on the backfill: a row whose
`price_changed_at_ns` is still 0 reads as its `first_seen`, so a backfill that
has not finished (or a row written by an older binary) still reads correctly.

SQLite: the existing `PRAGMA table_info` + `ALTER TABLE ADD COLUMN` path
(SQLite ADD COLUMN with a constant default is also metadata-only), and the
same batched backfill.

Rollback: an older binary ignores the new columns (it names its columns
explicitly) and its upsert leaves them as they are; on the way forward again
the read fallback covers any row it inserted.

## C. eBay end time

The Browse API `item_summary/search` response's ItemSummary carries
`itemEndDate` ("the date and time an auction listing will end; if a
fixed-price listing has ended, the date and time the listing ended", UTC) and
`buyingOptions` (`FIXED_PRICE`, `AUCTION`, `BEST_OFFER`, ...). Both are
returned by the plain search call: no `fieldgroups` value is needed (that
parameter only adds refinement/match-count containers).

Our production query sets no `filter`, and the Browse search returns ONLY
listings that have `FIXED_PRICE` among their buying options unless a
`buyingOptions` filter asks for auctions. So for our search an itemEndDate is
expected only on auction-with-Buy-It-Now listings; an active fixed-price
listing has no end the seller has committed to (a Good 'Til Cancelled listing
renews monthly), and an `itemEndDate` on one is not treated as a fact.

Rule: the connector decodes both fields; it sets `listing.Raw.EndsAt` (and so
`offers.ends_at_ns`) ONLY when `buyingOptions` contains `AUCTION` and
`itemEndDate` parses as RFC 3339. It also records `buying_options` as an
aspect. Everything else keeps the assumed rule (`not_seen_3_polls`). The 6h
content-age purge (License 8.1(b)) is unchanged; an end time does not extend
retention. Whether production returns `itemEndDate` for our query cannot be
checked from here (no credentials, and the call budget is the operator's): it
is decoded when present.

Deal submissions set `EndsAt` = the email's server arrival time
(INTERNALDATE, which the sender cannot set) + the lookback window, basis
`email_window`. A deal row written before this change has no `ends_at`; its
`expires_at` falls back to `first_seen + lookback`.

## D. Hiding expired listings

Hiding is done in the surface pipeline (`pipeline.Surface`), which every read
path shares (`search_items`, `/search`, `/watches`, `new_deals`,
`list_watches`):

1. Read up to `MaxCandidates` (5000) items from the item store, as before.
2. ONE batched read of those ids' offer summaries
   (`offer.Store.Summaries`, `WHERE id = ANY($1)` -- not N queries; the
   summary omits title/body/aspects).
3. Drop every item whose offer is EXPIRED, or whose SOURCE-STATED end
   (`ends_at`: an auction's end, a deal email's read window) has passed: an
   ended auction would otherwise stay visible until the next polls expire it
   (about 90 minutes at a 30-minute eBay interval). `get_item` still answers
   such a row, with `offer_note` saying it ENDED. An item with NO offer row is kept
   (nothing says it is gone; its timing fields are absent, which is visible
   to the agent).
4. If the store returned a full cap AND some candidates were dropped, the cap
   cut off active listings: re-read with the cap raised by the number dropped,
   at most 3 reads in all (logged when it still falls short). The store orders by
   `seen_at DESC` (last poll), and an expired listing by definition has not
   been polled for 3 intervals, so expired rows sort last and in practice the
   first read already holds every active candidate; the top-up makes the
   bound exact rather than probable.
5. Filter, valuate, score and rank only the survivors. Summaries ride along on
   each scored row, so the projection does not read the offer store again
   (this also replaces `withProductIDs`' one `Get` per row).

An offer-store read failure FAILS the request: without the status nagus
cannot tell a live listing from a gone one, and the safe direction is to show
nothing. (In production items and offers share one Postgres database, so an
offer outage is an item outage anyway.) `get_item` fails the same way.

`SurfaceResult` gains `HiddenExpired` (reported as `hidden_expired` in
`search_items`, `/search`, `new_deals`). `matched` now counts surfaceable
(non-expired) store matches.

Rejected alternatives: a store-level join (items and offers are separate
adapters; on SQLite separate files, and the item store must not depend on the
offer store); an excluded-id list passed into the item query (unbounded: a
year of churn is mostly expired ids).

## E. MCP tools

### `new_deals`

    since (required, RFC 3339)  category  watch  min_verdict  include_price_drops  limit

- `since`: must parse as RFC 3339, must not be more than 5 minutes in the
  future (clock skew), and must not be older than 30 days (a forgotten cursor
  should not dump the whole corpus; ask search_items for that). The response
  carries `as_of` (server time, RFC 3339) to pass as the next `since`.
- `category`: one configured category; absent = every configured category
  (rows merged by score; a category without offers, like `release`,
  contributes nothing because nothing there has a first sighting).
- `watch`: a watch name (see list_watches): restricts to that watch's
  category and text, and a row must clear the watch's STRONG criteria (what
  would ping). The watch's `limit` does not apply (it caps the watch's own
  inbox, not "what is new").
- `min_verdict`: `great` | `good` (default) | `market` | `poor` | `any`. The
  ladder is great > good > market > poor; `any` also admits unscored rows
  (`unknown-no-reference`, `unknown-no-price`, release verdicts). With
  `watch`, min_verdict applies only when given explicitly.
- `include_price_drops` (default true): a listing also counts when
  `price_changed_at >= since`, the change came after its first sighting, and
  its price went DOWN (`price_cents < previous_price_cents`). A row carries
  `new_reasons` in event order (`first_seen`, `price_drop`, or both) and
  `new_reason`, the latest. Both matter: a listing shown as new on one call
  can drop before the next, and the caller -- which overlaps windows --
  must see that drop. The skill keys a shown row on `id` + `price_changed_at`
  when `new_reasons` contains `price_drop`, and on `id` alone otherwise: a
  drop brings a listing back once, a rise (which also moves
  `price_changed_at`) does not (reviews of !37).
- `limit`: 1..100, default 25.

A category whose surface fails is listed in `failed_categories` and the
others still answer (as `/watches` isolates a failing watch); when every
asked category fails, the call is the fixed internal error. Only ACTIVE
listings with an offer row are considered. The answer reports what it
APPLIED: `watch` (the name, or null without one) and `min_verdict` (null
when a watch's strong criteria stood in for it). Each row is a search
row with the A fields plus `new_reason` (`first_seen` | `price_drop`). Rows are
per LISTING: the same product at two sellers is two rows; group on
`comparison_key` (or `product_id`), which the description says.

Race note: an item's `first_seen` is the start of the ingest pass that found
it, and that pass may still be writing when `as_of` is taken. Callers should
overlap windows (the skill uses `since = last as_of - 1h`) and de-duplicate
by row id.

### `list_watches`

    name (optional)

With `name`, the strong matches are capped at `limit` (1..100, default 25;
`strong_count` is the total, `strong_returned` the number sent; `limit`
without `name` is rejected, since the list form returns no matches): a watch
with no limit of its own (production's `cheap-big-hdd`) could otherwise send
every match, up to the candidate cap, into the agent's context.

Without `name`: every ACTIVE watch (not past `expires_at`) with name,
category, text, strong criteria (`strong_verdicts` -- defaulted to `["great"]`
exactly as evaluation defaults it --, `min_score`, `min_discount_pct`,
`new_within_days`), `audience`, `expires_at`, and its current
`candidate_count` / `strong_count` over ACTIVE listings. No matches. With
`name`: that one watch plus its strong matches as rows with the A fields.

Access: `audience` and `principal` are opaque tags nagus carries but never
interprets; the HTTP `/watches` surface applies NO per-caller filtering, and
`/mcp` has no caller identity (openclaw connects as one unauthenticated
in-cluster client). There is therefore no caller class to match an audience
against, and the MCP tool exposes the same watches `/watches` does. The
conservative part: `principal` (who asked, possibly a person's name) is NOT
returned -- `/watches` does not return it either and an agent does not need
it -- and expired watches are not listed.

### Schemas

Both tools: `additionalProperties: false`, strict decoding (the kit), read-only
annotation. `TestMCPToolSchemasUnchangedForOpenclaw` now pins all four
agent-facing schemas (search_items and get_item UNCHANGED; new_deals and
list_watches new), so a later change is deliberate.

openclaw's toolFilter must add the two names (gitops, after this merges):

    clusters/orac/apps/glovebox/configmap-openclaw-patches.yaml
    mcp.servers.nagus.toolFilter.include += "new_deals", "list_watches"

## Measured

Postgres 16-alpine (the CI digest) in podman on the operator's desktop,
1,000,000 offers in today's production shape (every column through
`vintage_mode`, its five indexes, 400-byte bodies, 25% expired): 882 MB.

| step | cost |
|---|---|
| `New` (schema + the three ADD COLUMNs) | 15 ms |
| one-statement backfill (first design, rejected) | 39.5 s; heap 781 MB -> 1563 MB until vacuum |
| batched background backfill, 5,000 per batch | 45.9 s; heap 781 MB -> 876 MB |
| upserts running concurrently with it (874) | p50 15 ms, p99 433 ms, max 1.4 s |
| backfill on a later startup (nothing to do) | 2.9 s, in the background |
| `Summaries` of 5,000 ids (one surface request) | 28-57 ms |

Production holds on the order of 10^4 offers, so there the backfill is well
under a second.

Rows hidden on production data (2026-10-03, read-only evidence only: the
per-category store match counts from `GET /search` and each source's latest
ingest pass from the pod log). Store matches that no source re-stored in its
latest pass are listings the source no longer shows -- expired, or about to
be. It counts ITEMS not re-stored, NOT offers with status expired, so it is
approximate. It is also an UPPER bound: a source whose fetch was incomplete skips expiry,
and its unseen listings stay active (and visible).

| category | store matches | re-stored by latest passes | would be hidden |
|---|---|---|---|
| hdd | 749 | 477 | ~272 |
| wine | 1380 | 1348 | ~32 |
| release | 2 | 2 | 0 (no offer layer) |

The exact figure needs the database:

    SELECT i.category, count(*) FROM items i JOIN offers o ON o.id = i.id
     WHERE o.status = 'expired' GROUP BY 1;

The container test (built image, Postgres 16, a two-phase eBay fixture:
phase 2 drops one listing, lowers one price and adds one) showed the dropped
listing hidden from `search_items`, `/search`, `/watches` and `new_deals`
(`hidden_expired: 1`), `get_item` answering it with `offer_status: expired`,
and `new_deals` returning exactly the new listing (`first_seen`) and the
cheaper one (`price_drop`, `previous_price_cents` 27900 -> 23900).

## Deferred

- Restock as "new" (an expired offer seen again keeps its first_seen):
  nagus-m8f. The opposite happens on eBay: an offer unseen for 6h is PURGED
  (License 8.1(b)), so a listing that reappears after that is a new row with
  a new first_seen and is reported by `new_deals` as `first_seen` -- relist
  semantics, and correct for eBay, where a relisted item is a new listing.
- Server-side price/verdict filters on search_items: nagus-1xy.
