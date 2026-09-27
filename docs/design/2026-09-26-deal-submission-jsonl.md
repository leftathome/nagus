# Deal submission by email: `nagus.deal/v1` JSONL on the deals mailbox (2026-09-26)

Bead: nagus-4uu. Related: nagus-239 (the deals mailbox), nagus-xgc (retailer
parsers), nagus-9xo (deferred: reply to the sender). Operator-approved design;
this document records it and the decisions made while building it.

## Why

nagus sees what its sources publish. It cannot see an in-store price, a deal a
friend mentions, or a retailer newsletter that is image-only (the real
wine.com and Newegg marketing mails carry no text products, nagus-xgc). The
household's humans and agents can. They already have a channel nagus reads,
the deals mailbox (`deals@` on the automation subdomain, IMAP, DKIM verified
by nagus itself). This adds a typed, deterministic way to put a deal into that
mailbox.

## Format: JSONL in the text/plain body

- The **text/plain** part is read line by line. A line that starts with `{`
  after trimming whitespace is ONE deal object. Every other line (greeting,
  signature, `Fwd:` headers, notes, quoted `>` replies) is ignored.
- Reading stops at the first reply/forward/signature boundary: a line that is
  exactly `-- ` (RFC 3676 signature), `-----Original Message-----`, a Gmail or
  Apple Mail forward marker, or an `On ... wrote:` attribution. So a reply
  that quotes an earlier submission never resubmits it, even when the client
  does not prefix quoted lines with `>`.
- A **hand-forwarded** message (a configured forwarder forwarding an
  allowlisted sender's mail) is read from the forwarded original, i.e. after
  the forward marker, and stops at the next boundary.
- Line numbers are the 1-based line of the text body as received. They are
  stable across re-reads, which is what makes them part of the identity.
- `format=flowed` (RFC 3676) text is un-flowed before parsing, so a client
  that soft-wraps long lines does not break them. A client that HARD-wraps a
  line breaks it: the fragment that starts with `{` is refused `bad_json` and
  the continuation is ignored. The status tool says so.
- **HTML-only messages are refused**, whole, with the message reason
  `no_text_part`. Decision: deriving text from HTML (entity decoding, `<wbr>`,
  smart quotes, client-inserted wrapping) makes the parse depend on the
  sender's mail client, which is the opposite of deterministic. Every client
  and every SMTP library can send text/plain.

### Limits

| Limit | Value | What happens past it |
|---|---|---|
| Deal lines per message | 50 | lines 51+ are refused `too_many_lines`; the first 50 are processed |
| Bytes per deal line | 4096 (after trimming) | that line is refused `line_too_long` |
| Message size | 4 MiB (connector default) | message skipped, counted `too_large` |
| Lookback | the source's `imapLookbackDays` (default 14) | older mail is not read; its status ages out |

Each line is decoded strictly and on its own: one bad line never sinks the
others.

## Spec: `nagus.deal/v1`

Canonical schema: `internal/deal/deal-v1.schema.json`, served read-only at
`GET /schemas/deal/v1.json`. It is GENERATED from the Go struct `deal.Deal`
plus a field-metadata table whose limits are the same constants the validator
enforces (`go test ./internal/deal -run TestSchemaFile -update` rewrites it);
`TestSchemaFileIsGenerated` fails if the committed file drifts from the struct,
and `TestSchemaMetaCoversEveryField` fails if a field has no metadata.

| Field | Req | Type | Rule |
|---|---|---|---|
| `category` | yes | string | `wine` or `hdd` |
| `title` | yes | string | 1-300 chars, no control characters |
| `price` | yes | decimal string or number | major units, `^[0-9]{1,7}(\.[0-9]{1,2})?$`, > 0, <= 1000000 |
| `url` | yes | string | `https` only, with a host, no userinfo, <= 2048 chars |
| `currency` | no | string | ISO 4217 `^[A-Z]{3}$`, default `USD` |
| `seller` | no | string | <= 100 chars; the store/merchant |
| `brand` | no | string | <= 100 chars; hdd: product hint; wine: the producer |
| `mpn` | no | string | hdd only; <= 64 chars |
| `gtin` | no | string | 8, 12, 13 or 14 digits |
| `vintage` | no | integer | wine only; 1800-2100 |
| `bottle_ml` | no | integer | wine only; 50-30000 |
| `capacity_tb` | no | number | hdd only; > 0, <= 1000 |
| `condition` | no | string | hdd only; `new`, `refurb`, `used`, `parts` |
| `note` | no | string | <= 1000 chars; stored, NEVER used for identity |
| `schema` | no | string | if present, exactly `nagus.deal/v1` |

- **Strict decode.** Unknown fields refuse the line (`unknown_field`), and so
  do case variants of a known key (`Title`): Go's decoder matches keys
  case-insensitively, so keys are checked against the exact names first.
  Trailing data after the object refuses the line (`bad_json`).
- A field that does not apply to the category (`vintage` on an hdd line,
  `capacity_tb` on a wine line) refuses the line (`field_not_for_category`),
  so a sender learns about a mistake instead of having data silently dropped.

### Mapping to a listing

Each accepted object becomes one `listing.Raw` on a per-category imap source,
so the EXISTING category extractors, the glovebox gate and the quark hint path
handle it unchanged:

| Deal | Raw |
|---|---|
| title | `Title` |
| note | `Body` (free text; the extractors may read it, identity never does) |
| url | `SourceURL` |
| price, currency | `PriceCents`, `Currency` |
| condition (hdd) | `ConditionRaw` (the hdd extractor now also accepts the normalized words) |
| seller | aspect `seller` -> `Offer.Seller` |
| brand, mpn, gtin (hdd) | aspects `brand`, `mpn`, `gtin` -> the offer's product hint, which is how quark resolves drives |
| capacity_tb (hdd) | aspect `capacity_tb` (the hdd extractor's typed capacity) |
| brand (wine) | aspect `wine_producer` -> with the title, the quark NAME hint |
| gtin (wine) | aspect `deal_gtin` (informational; a wine GTIN would mint a product beside the LWIN catalog's, see pipeline `NameHintProducer`) |
| vintage, bottle_ml (wine) | aspects `vintage`, `bottle_ml` (read by the wine extractor) |
| (principal) | aspect `submitted_by` |
| (message) | aspects `mail_message_id`, `mail_forwarded_by` (connector) |

A deal-jsonl wine source is opted in to name hints by its type: the sender
named the producer on purpose, which is the review the per-source `lwinStamp`
gate exists for. The global `NAGUS_LWIN_STAMP` switch still applies.

Every text field (title, note, and every aspect value, the principal
included) crosses the glovebox sanitize gate like any listing, fail closed. A
line the gate refuses is reported `gate_refused` and never becomes an item.
Its offer row IS recorded (the offer layer records every fetched listing
before the gate, and offers at rest are untrusted by design), but it is never
surfaced and the status tool gives it no offer id.

## Who may submit

- A source is `type: imap` with `imapParser: deal-jsonl-v1` and a sender
  allowlist `imapSenders` (a LIST), configured only in gitops. The repo holds
  no real address; tests and docs use `example.org` placeholders.
- Each sender is accepted only with a DKIM pass for its own domain (nagus's
  own verification, or the trusted MX's topmost Authentication-Results), the
  same rule as a single-sender source.
- `imapForwarders` keep their meaning: a household mailbox forwarding an
  allowlisted sender's message by hand vouches for it, and it is credited to
  that sender.
- The **principal** is the verified sender address, or its alias from
  `imapSenderAliases` (address -> short name, e.g. `caspar`). It is stamped on
  every offer as aspect `submitted_by`, next to the deal's own `seller`.
- Mail from anyone else is not read. The connector counts it
  (`nagus_deal_submissions_total{outcome="unknown_sender"}`) with a UID-only
  search: its bodies are never fetched.
- A message From an allowlisted address WITHOUT a DKIM pass is counted
  `unverified` and alerts (see below): it is either a spoof or a household
  domain whose DKIM broke, and both need a human.

### One source per category

A source binds one category's extractor, so deal submission is configured as
one source per enabled category (`deals-wine`, `deals-hdd`), all with
`imapParser: deal-jsonl-v1`. Every deal source reads the same messages; each
line is routed by its `category` to the source for that category, and the
others leave it alone. A line whose category is valid but has no deal source
is refused `category_not_enabled`. Startup refuses deal sources whose
`imapSenders`, `imapForwarders`, `imapSenderAliases`, `imapMailbox` or
`imapLookbackDays` differ: a line routed to a source that never reads its
message would stay pending forever.

The wine source must declare `wineChannel` and `origin` like every wine
source. For household tips that is a conscious choice (suggested: `retailer`,
the household's own jurisdiction), and it decides where the deal counts as
legal to ship.

## Idempotency, flags and lifetime

- Source key: `<Message-ID>#L<line>`. The offer and item id is
  `sha256(sourceID NUL key)[:16]`, so re-reading the mailbox updates the same
  rows and never duplicates. A resent message has a new Message-ID and is a
  new submission (documented for senders).
- The mailbox is opened with EXAMINE and bodies fetched with BODY.PEEK, like
  every imap source: nagus never sets `\Seen`, never moves or deletes. Humans
  can use the mailbox normally and nothing nagus does depends on flags.
- Items from deal sources are purged when not refreshed for
  `max(3 x interval, 24h)`: a deal surfaces while its email is inside the
  lookback window, then leaves. Offers expire (retained, not purchasable)
  three polls after they stop appearing, like every source.

## Status and counters

The ledger (`internal/deal.Ledger`) is in memory and rebuilt by every poll:
the connector re-reads the whole lookback window each time, so the ledger
always covers exactly that window and needs no table. After a restart it is
empty until the first poll, which runs at startup. Lines move
`pending -> accepted | rejected`; a transient failure (glovebox unreachable,
store error) leaves a line `pending` with a reason and it is retried next
poll.

Counters (bounded labels, all series pre-rendered at 0):

- `nagus_deal_submissions_total{outcome}` -- one per message, counted once per
  process at its first final outcome. `outcome` in: `accepted` (every deal
  line accepted), `partial`, `rejected` (had deal lines, none accepted),
  `empty` (no deal lines), `no_text_part`, `unknown_sender`, `unverified`,
  `too_large`, `invalid` (unparseable, no Message-ID, forward of a
  non-allowlisted sender).
- `nagus_deal_submissions_lines_total{outcome,reason}` -- one per deal line
  at its first final outcome. `outcome` is `accepted` (reason `none`) or
  `rejected` with a reason code from the table below.

Counters are per process lifetime: a restart recounts the window once, which
`rate()`/`increase()` treat as a reset.

### Reason codes (per line)

`line_too_long`, `too_many_lines`, `bad_json`, `unknown_field`,
`missing_field`, `bad_type`, `bad_schema`, `bad_category`,
`category_not_enabled`, `field_not_for_category`, `bad_price`,
`bad_currency`, `bad_url`, `bad_value`, `gate_refused`, `not_in_category`
(the category extractor says it is not a wine/drive, e.g. an SSD),
`extract_failed`. Pending reasons: `gate_unavailable`, `store_failed`,
`offer_store_failed`.

### Alert

`NagusDealSubmissionUnverified` (warning): any `unverified` submission in the
last hour. Justified because it is rare and needs a human either way (spoof
attempt, or a household domain's DKIM broke and its deals are being dropped).
No alert on rejections: a malformed line is the sender's problem and the
status tool tells them; no alert on `unknown_sender`: the mailbox is an open
channel and spam is expected. Gate outages are already covered by the
`nagus.sanitize` alerts.

## Discoverability

- **Schema:** `GET /schemas/deal/v1.json` (`application/schema+json`), the
  committed file byte for byte.
- **MCP tool `deal_submission_spec`** (read-only, no arguments):
  structuredContent `{schema_id, schema_url, schema, enabled, mailbox,
  enabled_categories, format, examples[2], example_body, when_to_use[],
  when_not_to_use[], limits{}, status_tool, reason_codes{}}`. The mailbox is
  the deal sources' `dealSubmitTo`, else `NAGUS_IMAP_USERNAME`; never a
  constant.
- **MCP tool `deal_submission_status`** (read-only): arguments
  `message_id` OR `principal` (+ `limit`, default 5, max 20).
  - By `message_id`: `{message_id, received, outcome, counts{accepted,
    rejected, pending}, lines: [{line, outcome, reason, category, offer_id,
    resolution, product_id}]}`; an unknown id is the kit's not-found result
    (`isError: true`). `outcome` is `pending` or a message outcome.
  - By `principal` (alias or address): `{messages: [{received, outcome,
    counts, lines: [{line, outcome, reason, category}]}]}`, newest first --
    no message ids, offer ids or product ids.
  - Neither or both arguments, or `limit` outside 1-20, is an invalid-
    arguments error; with no deal source configured the tool says deal
    submission is not enabled.
  - Never any line content: only line numbers, outcomes, reason codes, the
    category enum and ids.
- **Visibility decision.** nagus's MCP endpoint is unauthenticated and
  read-only; its only client is the openclaw gateway (household agents), and
  nagus cannot tell callers apart. So the capability is knowledge: a
  Message-ID (in the sender's Sent folder, not guessable in general) unlocks
  the full per-line detail with ids; a principal name unlocks only reason
  codes and counts for that sender's recent messages, which reveal nothing a
  sender wrote. The principal is never echoed by message-id lookups. If the
  endpoint ever gains per-caller auth, principal lookups should be limited to
  the caller's own principal.
- **Humans:** `docs/deal-submission.md`. **Agents:** the skill snippet in
  `docs/deal-submission-skill.md`.

## Rejected alternatives

- **CSV.** Commas are common in wine names ("Chateau X, Pauillac") and
  quoting rules vary by client; CSV is untyped (a price, a vintage and a GTIN
  are all strings) and has no per-row field names, so a missing column
  silently shifts every value after it. JSONL is typed, self-describing per
  line and strictly decodable.
- **A free-text LLM parse.** Not deterministic (the same mail can yield
  different deals on two polls, which breaks idempotency), and it puts
  untrusted mail text into an instruction context, which the nagus rules
  forbid: the deals mailbox is an open channel and the worst a malicious line
  may do is produce a wrong field value.
- **Replying to the sender by SMTP with per-line results.** Deferred to
  nagus-9xo: it needs SMTP credentials, loop and backscatter protection
  (never reply to an unverified or automated sender) and rate limits. Until
  then the read-only status tool is the feedback path.
- **Deriving text from HTML-only mail.** See Format.
- **A persistent status table.** The stateless connector re-reads the window
  every poll, so an in-memory ledger is always complete for the window; a
  table would add a schema to all three store backends for no extra
  information.

## Resumption notes

Worktree `/mnt/c/Users/steve/Code/nagus/.claude/worktrees/nagus-deal-jsonl`,
branch `nagus-deal-jsonl`. Progress is tracked in bead nagus-4uu's notes.
