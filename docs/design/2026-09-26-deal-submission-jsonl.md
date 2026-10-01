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
- **A submission is a fresh message.** A plain sender's message that carries
  `In-Reply-To` or `References` is refused whole, outcome
  `reply_not_accepted`: a reply carries a quoted original, and no pattern
  delimits that for every mail client and language. Forwards from a
  configured forwarder are exempt, because mail clients set those headers on
  a forward too (see "Security re-review"); they are read from the forward
  marker.
- As defence in depth, reading also stops at the first reply/forward/signature
  boundary: a line that is `--` or `-- ` (RFC 3676 signature),
  `-----Original Message-----`, an underscore rule of five or more, a `From:`
  line (plain, bold, or `Von:`/`De:`/`Da:`/`Van:`) with another header line
  within the next three, a Gmail or Apple Mail forward marker (quoted with
  `>` or not), or a line ending in `wrote:` or its German, French, Spanish,
  Italian, Dutch or Portuguese form (which also ends a wrapped
  attribution). So a reply
  that quotes an earlier submission never resubmits it, even when the client
  does not prefix quoted lines with `>`.
- A **hand-forwarded** message (a configured forwarder forwarding an
  allowlisted sender's mail) is read from the forwarded original, i.e. after
  the forward marker and its header block, and stops at the next boundary.
  The marker must be the FIRST boundary in the message and unquoted: one that
  appears below a reply boundary, a signature or a quote is part of someone
  else's text and opens nothing.
- A line that starts with a byte-order mark or another invisible character
  and then `{` is a deal line and is refused `bad_json`; it is not silently
  ignored. Bare-CR line endings split lines like LF.
- At most 256 KiB and 10,000 lines of a body are scanned. When that cap, and
  not a boundary, ends the read, status carries one `scan_truncated` entry;
  a line the cut went through is not read.
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
| Deal lines per message | 50 | the first 50 are processed; the rest are not decoded and become ONE `too_many_lines` entry (at the 51st deal line) carrying a `count` |
| Body scanned | 256 KiB, 10,000 lines | the rest of the body is not read; one `scan_truncated` entry |
| Ledger | 5,000 messages; 20,000 skipped-mail observations | the oldest are dropped |
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
| `title` | yes | string | 1-300 chars of visible text (see Text) |
| `price` | yes | decimal string or number | major units, `^[0-9]{1,7}(\.[0-9]{1,2})?$`, > 0, <= 1000000 |
| `url` | yes | string | exactly lower-case `https://`; printable ASCII; <= 512 chars; a lower-case DNS host of two or more labels whose last label starts with a letter (so no IP literal in any spelling) and is not `local`, `localhost`, `internal`, `lan`, `home`, `corp`, `svc`, `test`, `invalid`, `arpa`, `onion`; no userinfo, no backslash, no port but `:443`; every `%` a valid escape, nested at most twice. The schema's `pattern` is the same regular expression the validator uses (`deal.URLPattern`) |
| `currency` | no | string | ISO 4217 `^[A-Z]{3}$`, default `USD` |
| `seller` | no | string | <= 100 chars; the store/merchant |
| `brand` | no | string | <= 100 chars; hdd: product hint; wine: the producer |
| `mpn` | no | string | hdd only; <= 64 chars |
| `gtin` | no | string | 8, 12, 13 or 14 digits |
| `vintage` | no | integer | wine only; 1800-2100 |
| `bottle_ml` | no | integer | wine only; 50-30000 |
| `capacity_tb` | no | number | hdd only; >= 0.01, <= 1000 |
| `condition` | no | string | hdd only; `new`, `refurb`, `used`, `parts` |
| `note` | no | string | <= 1000 chars; stored, NEVER used for identity |
| `schema` | no | string | if present, exactly `nagus.deal/v1` |

- **Text.** Every text field (title, note, seller, brand, mpn, and the
  decoded url) may hold letters, marks, numbers, punctuation and symbols of
  any script and the plain ASCII space. Refused (`bad_value`, or `bad_url`):
  controls; format and other default-ignorable characters (bidi overrides and
  isolates, zero-width characters, soft hyphen, BOM, Hangul fillers,
  variation selectors, tag characters); every space but U+0020 (NBSP, the
  typographic spaces, U+2028/U+2029); the Braille blank; private-use,
  noncharacter, unassigned and replacement code points; a combining mark with
  no base or more than four on one base. Accented and non-Latin names pass.
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
| url | aspect `deal_url_text`, so the url crosses the gate as text: the host, then path+query+fragment percent-decoded until stable, then the same with every separator turned into a space. Decoding fails closed: a url the gate could not be shown in full is refused `bad_url`, never surfaced ungated |
| (principal) | aspect `submitted_by` |
| (message) | aspects `mail_message_id`, `mail_forwarded_by`, `mail_forwarded_from` (a claim) (connector) |

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
- Each sender is accepted only when NAGUS ITSELF verifies a DKIM signature
  aligned to the sender's own domain whose `h=` covers From. An
  Authentication-Results header is never consulted for a deal source (startup
  refuses `imapTrustAuthResults` on one), and is off by default for every
  imap source: the MX for the mailbox writes none, so the topmost one is
  whatever the sender wrote.
- A message with more than one From, Sender, Subject or Message-ID field is
  refused: DKIM verifies the bottom-most From its `h=` covers, so a second
  From prepended above a genuine signature would otherwise be the one read.
- `imapForwarders`: a household mailbox forwarding an allowlisted sender's
  message by hand. It is read (the forwarded original must name an
  allowlisted sender) but CREDITED TO THE FORWARDER, the only address DKIM
  verified; the original named in the forwarded text is unauthenticated and
  is recorded as aspect `mail_forwarded_from`, a claim.
- The **principal** is the verified address (sender or forwarder), or its
  alias from `imapSenderAliases` (address -> short name, e.g. `caspar`). It
  is stamped on every offer as aspect `submitted_by`, next to the deal's own
  `seller`.
- IMAP `SEARCH FROM` is a substring match; each candidate's parsed envelope
  From is checked exactly before its body is fetched.
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

- Source key: `deal-` plus 32 hex characters of a domain-separated SHA-256
  over (verified sender address, Message-ID, line). The sender writes its own
  Message-ID, so the id alone keys nothing: the ledger and the offer id are
  both scoped to the verified sender, and two senders using one Message-ID
  never share or overwrite an entry. The key is OPAQUE because an item's
  `source_key` is returned by `get_item`, `GET /item` and stored with the
  item: it must not reveal an allowlisted address, nor the Message-ID, which
  is the capability for the full status lookup. Nothing else on an ITEM
  carries either (asserted end to end); the address or alias
  (`submitted_by`), `mail_message_id`, `mail_forwarded_by` and the claimed
  `mail_forwarded_from` are aspects of the OFFER row, which no read surface
  returns. The offer and item id is
  `sha256(sourceID NUL key)[:16]`, so re-reading the mailbox updates the same
  rows and never duplicates. A resent message has a new Message-ID and is a
  new submission (documented for senders).
- Ordering and retention use the IMAP INTERNALDATE (`received`), which the
  sender cannot set, never the Date header.
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

`NagusDealSubmissionUnverified` (warning): the gauge
`nagus_deal_unverified_last_24h` is above zero, i.e. an `unverified` message
ARRIVED (INTERNALDATE) in the last 24 hours. A gauge over arrival time rather
than `increase()` of the counter, because a restart recounts the whole
lookback window and would re-fire for old mail. It resolves 24 hours after the
last such message. Justified because it is rare and needs a human either way (spoof
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
  - By `message_id`: `{messages: [{message_id, received, outcome,
    counts{accepted, rejected, pending}, lines: [{line, outcome, reason,
    category, count, offer_id, resolution, product_id}]}]}` -- one entry per
    verified sender that used that id (normally one). An id nagus never
    verified -- unknown, or From an allowlisted address without a DKIM pass --
    is the kit's not-found result (`isError: true`); the two are
    indistinguishable. `outcome` is `pending` or a message outcome.
  - By `principal`, an ALIAS only: `{messages: [{received, outcome, counts,
    lines: [{line, outcome, reason, category, count}]}]}`, newest first -- no
    message ids, offer ids or product ids. An address is never accepted (it
    answers the same empty list as an unknown name), so the tool cannot be
    used to test whether an address is on the allowlist. A sender with no
    alias uses its Message-IDs.
  - Output is bounded: at most 20 messages of at most 51 lines.
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

## Security review (rv35, 2026-09-27) and what changed

The independent review of MR !35 found the format and decoder sound and the
sender authentication and resource bounds not. Each finding has a regression
test that reproduced it before the fix.

| # | Finding | Decision | Test |
|---|---|---|---|
| C1 | A forged topmost Authentication-Results header was trusted | A-R trust is OFF by default for every imap source (`imapTrustAuthResults`, `Config.TrustAuthResults`); deal sources refuse to enable it | `TestForgedTopmostAuthResultsIsNotTrusted`, `TestAuthResultsTrustIsOptIn`, `TestDealHubValidation` |
| C2 | A duplicate From header bypassed DKIM | exactly one From, at most one Sender/Subject/Message-ID; the signature's `h=` must cover From | `TestDuplicateFromIsRefused`, `TestDuplicateIdentityHeadersAreRefused` |
| I1 | Unbounded ledger lines and status output | one summary `too_many_lines` entry with a count; scan caps; ledger cap; status at most 20 x 51 | `TestTooManyLinesIsOneBoundedEntry` |
| I2 | `url` never crossed the gate | `deal_url_text` aspect (gated); ASCII, 512, public DNS host only | `TestURLIsGated`, `TestDecodeHardening`, `TestInjectionIsRefusedByTheGate` |
| I3 | A gate-refused hdd line still sent brand/mpn/gtin to quark | `Ingester.HintsNeedGate` on deal sources: the hint is withheld unless the gate passed. "Never surfaced" now also means never sent to quark. Other hdd sources: nagus-voe | `TestInjectionIsRefusedByTheGate` |
| I4 | Incomplete reply-chain boundaries | Outlook rule and From/Sent block, wrapped Gmail attribution, quoted forward marker | `TestReplyChainBoundaries` |
| I5 | Message-ID alone keyed everything | keys are (verified sender, Message-ID) in the ledger and the offer id | `TestKeysIncludeTheVerifiedPrincipal` |
| N1 | A forward was credited to whoever its text named | credited to the forwarder; the named original is a claim | `TestForwardIsCreditedToTheForwarder`, `TestForwardedSubmissionIsCreditedToTheForwarder` |
| N2 | SEARCH FROM substring match fetched strangers' bodies | envelope From checked before the body fetch | `TestDisplayNameMatchIsNotFetched` |
| N3 | The status tool was an allowlist oracle | unverified is not recorded for status; principal lookup by alias only | `TestStatusIsNotAnAllowlistOracle`, `TestMCPDealSubmissionStatus` |
| N4 | Duplicate keys, BOM, invisible characters | duplicate key and BOM line are `bad_json`; Cf / default-ignorable / variation selectors are `bad_value` | `TestDecodeHardening`, `TestReplyChainBoundaries` |
| N5 | Sender-set Date used for ordering | INTERNALDATE | `TestReceivedIsTheInternalDate` |
| N6 | The mailbox grows forever | deferred: nagus-kvk (a retention/expunge story that keeps ingest read-only) | -- |
| N7 | The alert re-fired after a restart | alert on the `nagus_deal_unverified_last_24h` gauge | `TestUnverifiedGaugeIgnoresOldMail` |

Residual, by design: an allowlisted sender whose own mail account is
compromised can submit deals as that sender; the gate, the typed schema and
the extractors bound what such a deal can carry. A forwarder can make nagus
read any text it forwards, credited to the forwarder.

## Security re-review (rv35b, 2026-09-28) and what changed

| # | Finding | Decision | Test |
|---|---|---|---|
| NEW-1 | An invalid percent-escape dropped the url query from the gate text | invalid or over-nested escapes are `bad_url`; the gate also gets a separator-normalised view | `TestURLEscapesFailClosed`, `TestURLQueryEscapeCannotBypassTheGate` |
| NEW-2 | `source_key` exposed the sender address and Message-ID | opaque hashed key; items asserted free of both | `TestKeyIsOpaqueAndStable`, `TestItemsCarryNoSenderAddressOrMessageID` |
| NEW-3 | A forwarder's reply laundered a stranger's quoted lines | the forward marker must be the first boundary, unquoted | `TestForwardMarkerBelowAReplyBoundaryIsIgnored`, `TestForwarderCannotLaunderAQuotedStranger` |
| NEW-4 | Unverified observations keyed by Message-ID; duplicate-From never alerted | observations keyed by UID; a duplicate identity header counts as `unverified` | `TestUnverifiedObservationsAreKeyedByUID` |
| NEW-5 | U+2028 and friends accepted | character-class whitelist, combining-mark cap | `TestTextCharacterClasses` |
| NEW-6 | Reply formats read past | replies refused whole (`reply_not_accepted`); more patterns kept as defence in depth | `TestRepliesAreNotAccepted`, `TestARealisticReplyIsRefusedWhole`, `TestMoreReplyFormats`, `TestReplyHeadersAreReported` |
| NEW-7 | Loose host check | `deal.URLPattern` (schema and code agree) plus refused final labels | `TestURLHostIsAPublicDNSName` |
| NEW-8 | Lines past the scan cap vanished | one `scan_truncated` entry; invisible-prefixed lines are `bad_json` | `TestScanTruncationIsReported` |
| NEW-9 | `Ledger.once` unbounded | capped at 20,000, oldest arrivals evicted | `TestObservationsAreBounded` |
| NEW-10 | DKIM DNS lookups unbounded; re-verified every poll | per-lookup timeout (5s) and per-message budget (15s); a permanent refusal is remembered per UID for 24h (bounded), a temporary one is not | `TestDKIMLookupTimesOut`, `TestUnverifiedVerdictIsRemembered` |
| nit | DKIM alignment had no public-suffix floor | the signing domain must be the From domain or a parent no shallower than its registrable domain (`golang.org/x/net/publicsuffix`) | `TestAlignmentHasAPublicSuffixFloor` |

**The In-Reply-To / References rule.** Adopted for plain senders. The only
cost is that a sender cannot submit by replying in a thread, which is
documented and answered with its own outcome. It is NOT applied to
forwarders: Gmail, Apple Mail and Outlook all write `References` (and
usually `In-Reply-To`) on a forward so that it threads with the original, so
the rule would refuse every forward. That statement is from the clients'
known behaviour; it was not re-captured for this change (the opt-in
`TestRealCapturedMessage` is the place to confirm it against a real forward).
The forwarder path therefore keeps the marker logic, tightened per NEW-3.

**Accepted, no code change.** Five or more junk `DKIM-Signature` headers above
a genuine one make verification stop before reaching it (`MaxVerifications:
5`), so a legitimate message is refused. Adding headers needs modification in
transit, and the result is a refusal, never an acceptance; raising the bound
would only raise the work an unsigned flood can cause.

**Operator-visible only.** The connector's log line for a refused spoof names
the claimed address and domain. Logs are not an MCP surface.

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
