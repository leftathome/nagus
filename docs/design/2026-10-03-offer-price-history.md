# Offer price history, rolled up per product: `offer_price_history` and `price_history` (2026-10-03)

Bead: nagus-0ta. Builds on nagus-417 (`docs/design/2026-10-03-new-deals-offer-timing.md`:
`offers.price_changed_at_ns`, `previous_price_cents`, `ends_at_ns`). Follow-up:
nagus-xoy (a `new_low_within_days` watch criterion and a `new_low` new_deals
reason -- item 5 of the original design, deliberately not built here).

## Why

The operator wants to see how a product's price moves, across sellers, and
to let Caspar answer "is this actually a good price?". nagus-417 keeps ONE
step of history per offer (`previous_price_cents` at `price_changed_at`),
which can tell a drop from a rise but not "lowest in 90 days", "what did
seller X charge last month", or "is this the cheapest it has been". nagus owns
offers and prices (quark is identity only), so the history lives here.

## A. Schema

One new table, identical in shape on Postgres, SQLite and the memory store:

| column | Postgres | SQLite | meaning |
|---|---|---|---|
| `offer_id` | `TEXT NOT NULL` | `TEXT NOT NULL` | `offers.id` (no foreign key: see Retention) |
| `observed_at_ns` | `BIGINT NOT NULL` | `INTEGER NOT NULL` | from this instant the offer's price was `price_cents`, UTC Unix ns |
| `price_cents` | `BIGINT NOT NULL` | `INTEGER NOT NULL` | always > 0: an unknown price is never written |
| `currency` | `TEXT NOT NULL DEFAULT ''` | same | ISO 4217 as the source stated it with that price |

`PRIMARY KEY (offer_id, observed_at_ns)` (SQLite: `WITHOUT ROWID`). The key is
both the read path (one offer's points in time order, a range scan) and the
conflict target: two points of one offer at one instant are one point.

There are no other indexes. Retention deletes per source through
`offer_id IN (SELECT id FROM offers WHERE source_id = ...)`, which the primary
key serves per offer; a global `observed_at_ns` index would cost every insert
for a purge that runs once per poll over one source.

### What is written, and when

Rows are written by `offer.Store.Put`, IN THE SAME TRANSACTION as the offer
upsert, and only where the nagus-417 rule (`offer.Store.Put`) says the price
moved. `offer.HistoryRows` is the rule, shared by all three adapters:

| stored offer | incoming price | rows written |
|---|---|---|
| none (first insert) | P > 0 | `(first_seen, P)` -- the first sighting |
| none (first insert) | 0 (unknown) | none |
| price 0 (never known) | P > 0 | `(incoming last_seen, P)` -- the first known price (not a change for `price_changed_at`, but where the series starts) |
| price P | P (same) | none -- never a row per poll |
| price P | 0 (unknown) | none -- the offer keeps P (nagus-417) |
| price P, has history | Q > 0, Q != P | `(price_changed_at after the fold, Q)` |
| price P, NO history | Q > 0, Q != P | ANCHOR `(stored price_changed_at, P)`, then `(price_changed_at, Q)` |

Consequences, each covered by `internal/offer/offerstoretest/history.go`:

- **The newest point always equals the offer row**: its price is
  `offers.price_cents` and its instant is `offers.price_changed_at_ns`
  (`HistoryAgreesWithPriceChangedAt`, `HistoryConcurrentPutsOfOneOffer`).
- **A late, out-of-order poll** lands on the stamp `price_changed_at` keeps
  (it never rewinds) and REPLACES that point's price, exactly as it replaces
  the offer row's price. An anchor never overwrites (`ON CONFLICT DO
  NOTHING`). So a late poll whose computed stamp EQUALS an existing point's
  instant rewrites that point rather than adding one: history follows the
  nagus-417 offer row (last write wins at one instant), it is not an audit log
  of every poll's claim.
- **1000 -> 0 -> 1000 adds nothing; 1000 -> 0 -> 900 adds one point**, a
  change from the last known price.

**First-sighting row: written.** One row per new offer, at its first sighting
(or its first known price). Without it a timeline would begin at the first
CHANGE and the price an offer was listed at would exist only until the next
change overwrote `previous_price_cents`. It is a single row, and only for
offers created after this deploys.

**Seeding existing offers: not done.** No row is written for the offers that
exist at deploy. Instead, the first time such an offer's price changes, Put
writes an ANCHOR for the price it is leaving, at its stored
`price_changed_at`, before the change row -- lazy seeding, at most one extra
row per offer, only for offers whose price actually moves. Reasons:

1. A seed row says nothing the offer row does not already say: "price
   `price_cents` since `price_changed_at`". The read path (`offer.PriceAt`)
   already falls back to the offer row for an offer with no history, so the
   timeline of an unchanged pre-existing offer is the same with or without a
   seed.
2. The seed would launder an assumption into a recorded fact. For every offer
   older than nagus-417, `price_changed_at` was BACKFILLED to `first_seen`:
   nagus did not track changes before, so "this price since first seen" is
   not known. The lazy anchor makes the same assumption (the stamp is all
   nagus has), but only where it is needed to keep the price a change would
   otherwise lose -- and the design doc says so.
3. A bulk seed is a write over the whole table at startup. At production size
   (order 10^4 offers) it would be under a second, so cost is not the reason;
   but it would be one more startup backfill to batch and background (see
   nagus-417's measurements), for no information.
4. eBay seed rows would be purged within 6 hours anyway.

The same anchor also repairs an eBay offer whose rows the 6h window purged
while it stayed listed: its next change re-anchors the price it leaves. That
anchor carries the old stamp, so it is older than eBay's content window the
moment it is written; the read path never serves it (B, "eBay reads") and the
next retention pass deletes it. A retain-full offer never needs this: its
window keeps the carry-in point (B).

### Postgres write path

Before this change the Postgres upsert was ONE autocommit statement. History
needs the price the upsert REPLACED, and on PostgreSQL 16 `RETURNING` cannot
see the old row (`OLD` in `RETURNING` is PostgreSQL 18). So EVERY `Put` is now
an explicit transaction:

1. `BEGIN`
2. `SELECT price_cents, currency, price_changed_at ... FOR UPDATE` (a row
   lock, held to commit, so no concurrent Put changes the price in between)
3. only on a real price change: `SELECT EXISTS (...)` on the history table
4. the unchanged upsert, now with `RETURNING price_changed_at_ns, (xmax = 0)`
5. zero, one or two history `INSERT ... ON CONFLICT` statements
6. `COMMIT`

That is four round trips for an unchanged poll (BEGIN, SELECT, upsert,
COMMIT) where there was one, and up to seven for a change -- plus a row lock
held across them. At production rates (order 10^4 offers per pass, at most
hourly per source) this is well inside budget, but it is a real cost and the
first thing to revisit if ingest latency on Postgres grows.

Retries: when no row existed but the upsert UPDATED one (`xmax <> 0`: a
concurrent first insert of the same offer won), and when the server aborts the
transaction with `40P01` (deadlock) or `40001` (serialization failure), the
attempt -- rolled back whole -- is repeated, up to 4 attempts.

Lock order: Put locks the OFFER row, then writes history. ApplyRetention takes
locks in the SAME order: in one transaction it deletes the purged offers
first (`DELETE FROM offers ... RETURNING id`), then their history in a
SEPARATE statement (`DELETE FROM offer_price_history WHERE offer_id =
ANY($ids)`), and only then window-purges the surviving offers' history, which
takes no offer lock. The first version deleted history first and offers
second; a Put of a purged offer running alongside deadlocked (`40P01`).
Production runs one ingest goroutine per source with one replica, so it could
not happen there, but the store contract promises concurrent safety:
`HistoryConcurrentPutAndRetention` now exercises it on every adapter.

Why two statements: the second version did both deletes in ONE statement (a
data-modifying CTE), which runs on one snapshot. When the offers DELETE waited
on a Put's row lock and that Put committed a history row while its last_seen
was still older than the cutoff (a stale, out-of-order write), the offer was
deleted but the new history row was invisible to the same statement: an
orphan. A separate statement takes a new snapshot under READ COMMITTED and
sees it, and the deleted offer rows stay locked by the retention transaction,
so nothing new can commit for them in between.
`TestRetentionDeletesHistoryCommittedWhileItWaited` forces that interleaving
deterministically (an open transaction holds the offer's lock and writes a
history row; the test waits until `pg_stat_activity` shows the retention
DELETE blocked on a lock, then commits): it fails on the CTE and passes now.

SQLite already did a read-then-write in a transaction (single-connection
pool); it reads `currency` too and probes the history table only on a change.

## B. Retention

Retention stays a property of the SOURCE (`cmd/nagus.retentionForSource`), and
`ApplyRetention` -- still the only operation that deletes -- now deletes
history too, in one transaction with the offers. `offer.Retention` gains
`HistoryWindow`; `Retention.HistoryCutoff` decides:

- **eBay (`purge`, 6h, License 8.1(b))**: the history window IS the offer
  window. Every history row older than 6h is deleted whether or not its offer
  is still live (history is eBay Content too), and a purged offer's rows go
  with it explicitly (`offer_id IN` the offers being deleted). eBay timelines
  are therefore at most 6 hours long; `price_history` says so.
- **eBay reads**: the read path (`price_history`, `get_item`'s
  `recent_prices`) also drops any eBay point older than the 6h window
  (`sourcePolicy.historyMaxAge`, from the same retention table), so a row
  between retention passes -- or a lazy anchor written with an old stamp -- is
  never served.
- **Everything else (`retain-full`)**: rows older than
  `NAGUS_PRICE_HISTORY_RETENTION` (default `8760h` = 365 days; `0` = as long
  as the offer) are deleted per source -- EXCEPT each offer's CARRY-IN point,
  its newest row at or before the cutoff: the price still in effect when the
  window opens. A row goes only when a newer row at or before the cutoff
  supersedes it, so a price that stood still for longer than the window keeps
  its start (at most one extra row per offer). Offers are untouched. Chart:
  `offers.priceHistoryRetention` (chart 0.14.0). An invalid or negative value
  stops `nagus serve` at startup (`365d` is invalid: Go durations have no day
  unit) rather than silently keeping history for some other length of time.

No foreign key: SQLite does not enforce one without `PRAGMA foreign_keys`,
and an `ON DELETE CASCADE` on Postgres only would make the two adapters
behave differently. Offers are deleted only by `ApplyRetention`, which deletes
their history with them (Postgres: offers, then their history, in one
transaction; SQLite, whose single
connection serialises everything: the history first, in the same
transaction).

## C. Roll-up per product

Grouping is the one every row-returning tool already tells the agent to use:
`offer.ComparisonKey` -- quark's product id, plus the vintage for a wine whose
year matters (QUARK-04) -- and, within a key, ONE currency. A wine product id
spans its vintages, so `price_history` for a wine product id answers one group
per vintage; a vintage wine listing with no stated year has no comparison key
and is counted (`not_comparable_offers`), never compared. The key needs the
listing's extracted vintage, which lives on the ITEM (item id == offer id);
only offers whose product has a vintage mode pay for that item read.

Per group:

| field | definition |
|---|---|
| `current_lowest_price_cents`, `current_lowest_item_id` | lowest known price among PURCHASABLE offers: active and any source-stated end still ahead (the surface's hiding rule). Expired offers are history only. |
| `all_time_low_cents`, `all_time_low_item_id` | lowest `min_price_cents` among the group's offers, expired included (the lifetime low each offer was seen at) |
| `all_time_low_at` | the earliest recorded point at that price; ABSENT when the low predates the recorded history (an offer older than the table, or rows aged out) -- never guessed |
| `price_days_ago_cents` | lowest price in effect `days` ago among offers on offer then (`offer.PriceAt`) |
| `change_vs_days_ago_cents`, `_pct` | current lowest minus that; negative = cheaper now; absent when either side is unknown |
| `sellers[]` | offers seen inside the window, active first then cheapest: current price, `price_changed_at`, `price_at_window_start_cents`, and the points inside the window |

`offer.PriceAt(summary, history, t)` is best-effort and says so: no price
before `first_seen` or after an expiry; else the latest point at or before `t`;
with NO history, the offer row's `price_cents` from `price_changed_at` on and
UNKNOWN before it; before the earliest retained point, unknown. It does NOT
use `previous_price_cents` for the time before `price_changed_at`: that is the
price the last change left, which held only since the change before -- an
unknown time. (The first version did, and an eBay listing with two changes
and its history purged reported the middle price for a time it had the first,
and a wrong `change_vs_days_ago`.) Unknown makes `price_at_window_start_cents`,
`price_days_ago_cents` and the change fields absent rather than wrong.

## D. Agent surface

### `price_history` (new MCP tool)

    product_id | item_id (exactly one), days (1..365, default 30)

- `product_id` takes a row's `product_id` OR its `comparison_key`
  (`<product id>@<vintage>`): the latter answers that one group.
- `item_id`: a quark-resolved listing answers its product's group (its own
  comparison key); an unresolved or non-comparable listing answers its own
  history only (`scope: "listing"`).
- Answer: `as_of`, `days`, `window_start`, `scope`, `product_id`,
  `comparison_key` (null unless asked or derived), `products[]` (the groups
  above), `groups_truncated`, `offers_truncated`, `not_comparable_offers`.
- Hard caps: 50 offers read per product (most recently seen first), 10
  groups, 20 sellers per group, 30 points per seller (newest), 500 history
  rows read per offer to date the low; each cut is flagged.
- Read-only, strict arguments, values only in structuredContent with the
  shared untrusted-text note. A missing product or item is the kit's
  not-found result; with the offer layer off it is invalid argument.
- **No offer free text, ever.** Offers at rest are UNGATED (the glovebox
  crossing happens at the point of use, `internal/offer/offer.go`), and a
  listing glovebox refused -- or any offer-only listing -- is still recorded
  as an offer. So nothing seller-authored is taken from an offer: no seller,
  title, body or aspect (a seller label was in the first version and was
  removed in review). A seller timeline is identified by `item_id` (a hash)
  and `source_id` (configuration); `get_item` returns the gated item. The
  currency, also a source string, is served only when it is three ASCII
  capitals (ISO 4217 shape), else empty -- in `price_history` and in
  `get_item`'s `recent_prices`. `comparison_key` is echoed only when it
  matched stored offers, never as bare caller input.
  `TestPriceHistoryNeverServesUngatedOfferText` puts injection text in every
  free-text field of a refused deal offer and checks both blocks of both
  tools.

Its input schema is pinned in `TestMCPToolSchemasUnchangedForOpenclaw`.
**openclaw's toolFilter must add `price_history`** (gitops, after this merges):

    clusters/orac/apps/glovebox/configmap-openclaw-patches.yaml
    mcp.servers.nagus.toolFilter.include += "price_history"

### `get_item` / `/item`

Gain `recent_prices`: the listing's newest 5 recorded points (`at`,
`price_cents`, `currency`), oldest first; absent when none are recorded or the
offer layer is off. A history read failure omits it (logged): unlike the
offer status, it is enrichment. The input schema is unchanged.

## E. Storage and migration cost

**Migration**: `CREATE TABLE IF NOT EXISTS` in the schema each adapter runs on
open. Catalog-only on both backends, independent of the offers table's size;
no backfill, no seeding (A). An older binary ignores the table; rolling back
and forward again leaves a gap in the history for the rolled-back period,
nothing worse.

**Growth** (reasoned, not measured on production: no database access from
here). Production (gitops `helmrelease-nagus.yaml`, 2026-10-03) polls 3 drive
storefronts (serverpartdeals and waterpanther hourly, savemyserver 6-hourly
and offer-only, up to 24 pages), eBay every 30 minutes, about 25 small winery
catalogues every 12 hours (27-62 offers each), a TTB feed with no offers, and
two deal mailboxes; the offer table holds on the order of 10^4 offers
(nagus-417: 749 hdd and 1380 wine store matches, plus offer-only sources).
Rows per day are first sightings of new offers plus real price changes:

| source class | offers | assumption | rows/day |
|---|---|---|---|
| winery catalogues | ~1,400 | prices move at releases, ~0.2%/day; a few new SKUs | ~5-20 |
| drive storefronts | ~2,000-7,000 | repricing 1-10%/day of the catalogue, plus new SKUs | ~50-700 |
| deal mailboxes | tens | one first-sighting row per deal | <10 |
| eBay | live listings only | purged at 6h | steady state: at most a few hundred rows in the table |

Central estimate a few hundred rows a day; a deliberately pessimistic bound
1,000 rows a day.

Measured on Postgres 16-alpine (the CI digest) in podman on the operator's
desktop, the shipped table shape, 1,000,000 rows (100,000 offers x 10 points,
16-hex ids as `DeterministicID` makes them):

| measure | value |
|---|---|
| heap | 76.6 MB (76.6 B/row) |
| primary key index | 65.8 MB (65.8 B/row) |
| total | 142.5 MB, **142.5 bytes per row** |
| bulk insert of the 1,000,000 rows | 6.4 s |
| `price_history`'s read: 50 offers x newest 500 rows (500 rows back) | 1.7 ms execution, a bitmap scan of the primary key |

So, at the 365-day default window:

| rows/day | rows held | Postgres size |
|---|---|---|
| 300 (central) | ~110,000 | ~16 MB |
| 1,000 (pessimistic) | ~365,000 | ~52 MB |

plus at most one carry-in row per retain-full offer (order 10^4 rows, ~1.5
MB).

The offers table it sits beside was 882 MB at 1,000,000 offers (nagus-417's
measurement), so history stays a small fraction of the offer layer. On SQLite
the table lives in the offer database file, which shares the PVC with the
item store (values.yaml: "NOTE ON GROWTH"); SQLite stores it more compactly
(`WITHOUT ROWID`, the key is the table). Lower the window with
`offers.priceHistoryRetention` if the volume is tight.

## Known limits

- **No index for the purge.** The window purge finds a source's rows through
  `offer_id IN (SELECT id FROM offers WHERE source_id = ...)` and the primary
  key; the carry-in check is a per-row primary-key probe. At the estimated
  size (10^5 rows) this is cheap; if the table grows by orders of magnitude,
  add an `(observed_at_ns)` index or a denormalised `source_id` column.
- **The all-time low's currency** is the group's: groups are per currency,
  so a low is never compared across currencies, but `min_price_cents` on the
  offer row does not record which currency it was seen in. An offer whose
  source switched currency would carry a low from the old one. No production
  source does.
- **Orphans** (closed in review). A stale Put racing the purge of its own
  offer could leave a history row with no offer while the purge was one
  statement; it is two now (B, "Why two statements"). The remaining
  interleavings re-create the offer instead: a Put that waits on the
  retention transaction's lock finds the row gone and inserts it afresh, with
  its history. No orphan path is known; none is cleaned up (no periodic
  `NOT IN offers` sweep), so a new one would persist unread.
- **History starts at deploy.** Timelines of existing offers begin at their
  first change after it (with the anchor), or are the offer row alone.

## Rejected alternatives

- **A row per poll.** Multiplies writes by the poll count (eBay every 30
  minutes) for no information; the rule above already records every change.
- **History derived from `previous_price_cents` alone.** One step, and lost on
  the next change.
- **A JSON array column on `offers`.** Rewrites the offer row on every change,
  unbounded row growth, and no per-row retention.
- **Foreign key with `ON DELETE CASCADE`.** Postgres-only behaviour; see B.
