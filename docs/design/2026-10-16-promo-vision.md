# Reading image-only retailer promo mail: `promo-vision-v1` (2026-10-16)

Bead: nagus-3ua (supersedes nagus-xgc). Related: nagus-yfl (deal submission in
production), nagus-4uu (the `nagus.deal/v1` format this reuses).

## Why

The deals mailbox holds retailer newsletters that yield no offers because the
products are pictures: on 2026-10-16, 25 Newegg mails (product tiles are
static JPEGs whose `alt` is the Newegg item number), 17 wine.com mails
(personalized recommendation images rendered at OPEN time by RichRelevance)
and 6 Total Wine mails (live Movable Ink images). The text parts carry only
order-level promo codes. A probe on the shared GPU (`qwen3.6-35b-vllm-nothink`
behind LiteLLM, OpenAI-compatible, temperature 0) read title, price and
was-price exactly from a Newegg tile and a wine.com image in 1-8 s each.

Operator decisions: nagus does this automatically, with no agent in the loop;
it may fetch the open-time images and follow each tracked link ONCE to reach
the product page; it stores only the clean canonical product URL. Tracking
tokens are never stored and never reach an agent.

## Shape

A new imap parser, `promo-vision-v1`, selected per imap source like
`deal-jsonl-v1`. **One source per retailer**, because legality (wine
`wineChannel`/`origin`) and the principal are per seller:

```
imap connector (DKIM-verified mail from imapSenders only)
  -> promo parser: profile -> select images -> fetch -> vision read x2
     -> agreement guard -> canonical product URL -> nagus.deal/v1 line
     -> deal.Decode (the SAME strict validator) -> deal.ToRaw
  -> promo.Source: per-poll budget, dedupe by product, completeness
  -> the existing ingester: offer layer, glovebox gate, category extractor
```

Package `internal/promo` owns everything new; `cmd/nagus/promo.go` wires it.

### Source config (gitops)

```json
{"name": "promo-newegg", "type": "imap", "category": "hdd",
 "imapParser": "promo-vision-v1", "promoProfile": "newegg",
 "imapSenders": ["<the retailer's newsletter From address>"],
 "intervalMinutes": 60}
```

Startup refuses a promo source whose profile is unknown, whose category is
not `wine`/`hdd` (the `nagus.deal/v1` categories), that has no `imapSenders`,
any sender outside the profile's `senderDomains`, `imapFrom`,
`imapForwarders` (a forward would be credited to a household member while the
offer claims the retailer), `imapTrustAuthResults` or a fixture. A wine promo
source needs `wineChannel` and `origin` like every wine source. Real sender
addresses live in gitops only; code and tests use `example.com`.

### Profiles are data

`internal/promo/profiles.json` (embedded) ships `newegg`, `winecom` and
`totalwine`. A profile names: `seller`, `senderDomains` (a sender must be on
or under one), `imageHosts` (the only hosts an image is fetched from),
`productAlt` (optional regex: only images whose alt matches are candidates,
and its group is the retailer item number), `linkHosts` (tracked redirectors
a resolution may start at), `redirectHosts` (hosts a redirect may pass
through), `productHosts` + `productPath` (what a final product URL must
look like), `canonical` (`newegg-item` or `follow-link`), `category` (a
source using the profile must declare exactly this category; startup refuses
another) and `maxImages`.

## Image selection

The HTML part is parsed with `golang.org/x/net/html`. An `<img>` is a
candidate only when its (http-upgraded) host is in `imageHosts`, it is not
hidden (`display:none`, `visibility:hidden`), neither width nor height hint
(attributes or inline style px) is under 60, its alt is not navigation or
branding (logo, social networks, app stores, preferences, unsubscribe, ...),
and, for a `follow-link` profile, it sits inside a link on a `linkHosts`
host. A `productAlt` profile admits only matching alts. Duplicates (same src)
count once; at most `maxImages` (default 24) per message are taken, the rest
counted `over_cap`.

After the fetch, an image under 2 KiB or whose decoded dimensions (JPEG, PNG,
GIF) are under 80 px either way is dropped: pixels and spacers.

## Fetching (images and links)

- https only. An `http://` image or tracked link on an allowlisted host is
  upgraded to https before it is requested; a redirect to http is refused
  (`http_downgrade`). No userinfo, no port but 443, no IP literal.
- The dialer refuses loopback, private, link-local, multicast and unspecified
  addresses after DNS resolution, so an allowlisted name that resolves
  inward cannot reach the cluster.
- Images: at most 2 MiB (a larger body is refused, not truncated),
  `Content-Type` and the sniffed type must both be `image/jpeg|png|gif|webp`,
  20 s timeout, two images at a time, at most 2 retries on a transport error
  or 5xx. Image redirects must stay on `imageHosts`.
- No cookie jar anywhere; nothing persists between requests.

## Reading: vision with a hallucination guard

`NAGUS_VISION_BASE_URL` (OpenAI-compatible root, e.g. LiteLLM's `/v1`),
`NAGUS_VISION_API_KEY`, `NAGUS_VISION_MODEL`. With any unset the feature is
OFF: promo sources still poll IMAP but fetch nothing, call nothing, emit
nothing, report their fetch incomplete (so nothing expires) and log once per
poll. The key is sent only as the `Authorization` header to the vision
endpoint and is never logged; errors are scrubbed of it.

Each image is read TWICE with two independently worded prompts, temperature
0, image as a `data:` URL. Each answer must be exactly one JSON object
`{"products":[{"title","price","was_price","brand","currency"}]}` (code
fences tolerated; unknown fields, a key spelled with other case, a
duplicated key, trailing data or more than 8 products refuse the read). The guard keeps a product only when:

- both reads found exactly one product (`no_product` / `multi_product`
  otherwise: a multi-product banner has one link, so its products cannot be
  linked one by one);
- both prices parse (a leading `$`/`US$`/`USD` is stripped; thousands
  commas only in their proper places, `1,299.99` yes, `12,99` no; then the
  `nagus.deal/v1` price rule) and are EQUAL (`price_disagree`);
- the titles agree roughly -- token overlap (Jaccard over lower-cased
  alphanumeric tokens) at least 0.5 -- AND every number in the stored title (read A's) appears in read B's
  (`title_disagree`; B may add one A lacks, such as "7200 RPM" or "750ml"): "IronWolf 8TB" vs "IronWolf 18TB" or a 2021 vs a 2022
  vintage are different products even when the words agree, and the
  capacity and the vintage are what the deal is judged on;
- currency is USD or unstated in both (`currency`);
- a was-price kept only when both reads give the same one; it must exceed
  the price (`price_sanity` drops the product otherwise), and one above 5x
  the price is not believed (the product is kept without it).

The title and brand are reading A's (clipped to the deal format's 300
characters); brand only when both reads agree.

Both prompts say which price is THE price when two are printed (the sale
price, not the struck-through one). The first live run showed why: prompt A,
worded only "the current price", swapped wine.com's `$27.97` / was `$29`
while prompt B read them right -- the guard dropped the product
(`price_disagree`), as it should, and the wording was fixed.

A transient failure (image or link 5xx / transport error, a vision endpoint
5xx, 408, 429) leaves the image pending for the next poll, at most 3
attempts. A 401/403 is the deployment's key, not the image: the poll's reads
stop, the unspent calls are refunded (both at read A, read B's at read B), no attempt is counted, the fetch is
incomplete and one line per poll is logged -- a bad key never marks the
window as read. an
answer that is not the strict JSON shape, or any other 4xx, is final
(`vision_failed`).

### Budgets (the GPU is shared)

`NAGUS_VISION_MAX_CALLS_PER_POLL` (default 60, per source poll) and
`NAGUS_VISION_MAX_CALLS_PER_DAY` (default 600, all promo sources, UTC day).
An image reserves both of its calls together, after its fetch succeeded (a
failed fetch costs nothing; a quick availability check first keeps a spent
budget from fetching the window on every poll), so a budget never leaves half
a guard; when read A fails, read B's reservation is refunded. Messages are
handed to the parser NEWEST FIRST, so a budget is spent on current mail.

A budget refusal is counted (`nagus_promo_vision_budget_exhausted_total`)
whether the availability check or the reservation refused it.

The budget is IN MEMORY: a restart starts a fresh day's allowance and
re-reads the window, so N restarts in a day can spend N times the daily
budget. Persisting used-today (and the reading cache) is nagus-p7x. When a budget runs out the rest of the message is DEFERRED: retried on
a later poll, the fetch reported incomplete so no offer expires meanwhile.

## Product URL

- `newegg-item`: built from the alt's item number without touching the
  tracked link. `34-156-677` -> `https://www.newegg.com/p/N82E16834156677`
  (Newegg's own product URLs; slug-less form). A parent code
  `1TS-000E-1DFS6` -> `https://www.newegg.com/p/1TS-000E-1DFS6`. A
  marketplace listing `9SIC11UKGE3529` has no slug-less `/p/` page of its
  own (its pages are `/p/<parent>?Item=9SI...`), so it maps to Newegg's
  item-number entry point `https://www.newegg.com/Product/Product.aspx?Item=9SIC11UKGE3529`:
  the one query parameter kept anywhere, and it is the item identity, not
  tracking.
- `follow-link`: the enclosing tracked link is followed once, by hand: at
  most 5 hops, each https, each on `linkHosts`/`redirectHosts`/
  `productHosts`, no loops, no cookies, response bodies discarded. As soon as
  a Location (or the final URL) is on a `productHost` and its path matches
  `productPath`, resolution stops WITHOUT requesting the product page (they
  sit behind bot protection) and the URL is canonicalized: https, lower-case
  host, path only (no query, no fragment, no trailing slash). The path
  rules are bounded (`/product/<slug up to 120>/<id up to 10 digits>`;
  Total Wine at most 5 directories), and a slug segment carrying a run of 8+
  digits or 16+ hex characters (an id, possibly a subscriber's) is refused.
- Link vs image: an open-time image and its tracked click are rendered
  separately and can name different products (right price, wrong URL), and
  a producer's other wines share most words ("caymus-special-selection-
  cabernet-sauvignon" vs "Caymus Cabernet Sauvignon"). For `follow-link`
  profiles the canonical URL's slug (wine.com `/product/<slug>/<id>`, Total
  Wine `.../<slug>/p/<id>`) must match the agreed title:
  - SIZES equal on both sides (1.5 L, `1-5-liter`, magnum, 375 ml / half
    bottle, an N-pack; the standard 750 ml is no marker): a size on one
    side only refuses;
  - a year compared when both state one; for a profile whose slugs carry
    the vintage (`slugHasVintage`, wine.com) a title year missing from the
    slug refuses;
  - other numbers (Penfolds Bin 389 vs Bin 707, a cuvee number) compared
    like years: when both sides carry some, they must be equal;
  - CONTENT tokens (accent-folded; stopwords such as the/by/al/de/di/la/le/
    and, numbers and size words removed) never differing in a VARIANT
    marker (rose, riserva, reserva, reserve, gran, extra, brut, blanc,
    noir, sec, demi, ...);
  - Jaccard >= 0.75, or the only difference is ONE producer suffix on the
    slug side (vineyards, winery, wines, estate, cellars, chateau,
    domaine): `caymus-vineyards-cabernet-sauvignon-2021` matches "Caymus
    Cabernet Sauvignon 2021", but `opus-one-2019` does not match "Overture
    by Opus One 2019" nor `ridge-zinfandel` "Ridge Geyserville Zinfandel";
  - two content tokens on the slug, one of them not generic (wine, red,
    white, blend, ...: `red-wine` is a category page).
  Otherwise `link_title_mismatch`.
- Anything else
  (`link_offsite`, `http_downgrade`, `redirect_loop`, `too_many_redirects`,
  `link_unresolved`) drops the product; a tracked URL is never stored.

## Category fit (hdd)

A Newegg newsletter is mostly laptops, monitors and GPUs, and the hdd
extractor -- written for drive-only feeds -- refuses SSDs but not laptops. So
a product for an hdd promo source must ALSO look like a drive before it is
emitted (and before any link is followed): a capacity (`12TB`, `960GB`), a
drive word (hard drive, HDD, SATA/SAS, rpm, a drive line such as IronWolf or
WD Red) and no computer word (laptop, notebook, monitor, GPU, enclosure,
gaming desktop/PC, ...). "desktop", "pc", "computer" and "tower" are also
how drives are sold ("WD Blue 4TB Desktop Hard Disk Drive", "... for Computer
Desktop PC"), so they refuse a title only when it has no strong drive phrase
(hard drive, hard disk drive, HDD). A prebuilt PC or NAS lists its drive
too, so computer SPECS -- read on the FULL title, purpose clause included
-- always refuse (also PC lines such as Inspiron, OptiPlex, IdeaCentre,
Pavilion, iMac, Mac mini, Veriton, Precision, any workstation or mini PC,
Xeon or an Intel CPU model, and NAS boxes such as LinkStation, My Cloud,
DiskStation, Asustor Drivestor); a drive bay count ("2-Bay") refuses outside
the purpose clause, and "for 1-24 Bay NAS" is a purpose clause (`16GB RAM`/DDR, Core i3-i9, Ryzen,
Windows 10/11, `2-Bay`, Synology, QNAP, a NAS that is not a "NAS (hard)
drive") and so do "desktop PC", "desktop computer" and "tower desktop". The
rules read the title with a trailing purpose clause ("... for Computer
Desktop PC") removed: it says what the drive is for, not what it is. Drop-only (`not_in_category`); the extractor still judges what passes.
Wine has no such pre-filter: the wine extractor's own non-wine rules decide.

## Into the deal pipeline

Each kept product becomes a `nagus.deal/v1` line (category = the source's,
title, price, was-price, `url` = canonical, `seller` = the profile's,
`brand` when agreed) and goes through `deal.Decode`, the same strict decoder
human submissions pass, then `deal.ToRaw`. A refused line is dropped with
`deal_invalid`. The listing then meets the glovebox gate and the category
extractor in the ingester exactly like a submission: a Newegg laptop on the
hdd source is `not_in_category` and counted.

Provenance on the OFFER: aspects `submitted_by` = `retailer:<profile>`,
`offer_origin` = `email_image`, `promo_profile`, `vision_model`,
`compare_at_cents` (the agreed was-price; the wine extractor's discount),
and `mail_message_id` -- which on a promo offer is NOT the retailer's
Message-ID (mailing services encode subscriber and campaign ids in it) but a
short non-reversible tag of it (`msg-` + 16 hex of a SHA-256). Agent-facing rows of a promo source
carry `offer_origin: "email_image"`, `submitted_by: "retailer:<profile>"`
and a fixed `offer_origin_note` saying the price was machine-read from a
retailer's email image and should be confirmed at the link -- lower trust
than a catalogue feed.

Idempotency: the source key is `promo-` + 32 hex of SHA-256 over (profile,
canonical URL), so the same product in a later mail is the SAME offer (last
seen and price updated, not duplicated). The source wrapper dedupes one
poll's products by key, keeping the one from the newest mail. Each message is
processed once per process: results per image are cached by (sender,
Message-ID, image index) for the lookback window plus two days (bounded), so
a re-poll reuses them and only deferred or transiently failed images are
retried. The two READINGS of an image are also cached by the SHA-256 of its
bytes ACROSS messages, so a tile repeated in later mails costs no call.
Both caches are in memory (nagus-p7x).

Wine promo sources may carry `wineShipsTo` (an operator declaration from the
seller's own shipping policy that replaces the computed legal-destination
set; see the wine design doc): wine.com is origin US-CA but ships
nationwide, so `promo-winecom` declares `wineShipsTo: [US]`.

Expiry basis is `email_window` like deal submissions (arrival + lookback);
items purge after `max(3 x interval, 24h)` unseen.

Fixed on the way: the wine bundle's channel tagger wrapped every wine
connector WITHOUT forwarding `FetchComplete`, so every wine source read as
complete and a partial fetch (a truncated catalogue walk, or here a deferred
promo poll) could expire offers nagus had not reached. It forwards it now. Behaviour change for EXISTING wine sources: one whose
fetch is incomplete (a Shopify walk truncated at maxPages, a rate-limited
catalogue) now skips offer expiry AND retention for that poll, every poll it
stays incomplete -- the ingester logs "fetch was incomplete; skipping offer
expiry". Production logs for the 72 h before this change had no truncation
lines, so no current source is affected.

`deal_submission_status` does not cover retailer principals: the promo
parser does not use the submission ledger (its senders are retailers, not
household principals, and its per-line semantics differ). Use the metrics
and the per-message log line instead.

## ARC trust (nagus-25g, operator decision 2026-10-17)

In production the Newegg and Total Wine copies in the deals mailbox were
REFUSED "without a verified DKIM pass": ForwardEmail's MX verifies DKIM on
receipt, but its IMAP STORAGE then alters the body, so nagus's own check of
the stored copy fails (`dkim: body hash did not verify`). Every such
message carries a valid ARC chain: i=1,2 by google.com (the operator's Gmail
auto-forward), i=3 sealed by `d=forwardemail.net`, whose
ARC-Authentication-Results (authserv-id `mx1.forwardemail.net` /
`mx2...`) records `dkim=pass header.i=@<sender domain>`. (wine.com's stored
copies still verify directly.)

An imap source may opt in with `imapTrustARCSealers`,
`imapTrustARCAuthservIDs` and `imapTrustARCRecipients` (all three required;
`imapTrustARCMaxAgeMinutes`, default 60; OFF by default; promo and
deal-jsonl sources only, deal sources must agree; a promo source still
refuses `imapTrustAuthResults`). Order: direct DKIM exactly as before; only
if it fails, and only on an opted-in source, the ARC path.

What binds what (and why both checks are needed):

- The top ARC-Seal signs ONLY the ARC header fields of instances 1..N
  (AAR, AMS, AS). It proves the sealer wrote that verdict and that AMS, but
  a seal copied onto another message still verifies: alone it would let
  anyone who ever received a sealed message (any ForwardEmail alias) swap
  its Subject and body and send it to deals@ (security review of !42).
- The top ARC-Message-Signature signs the message header fields in its h=
  (From, To, Subject, Date, Message-ID, ...) and the body hash bh= as the
  sealer computed it on receipt. nagus verifies the AMS signature over THIS
  message's headers using bh= AS WRITTEN (it does not recompute the body
  hash, which storage broke), and requires h= to cover from, to, subject
  and date. The BODY is therefore not bound; the headers are.
- The trust anchor is the sealer's DNS key: for ForwardEmail
  `default._domainkey.forwardemail.net`, an RSA 1024-bit key (the
  verifier's minimum).

Checks (`internal/arc`, RFC 8617, then `imapmail`):

- the ARC sets are a contiguous 1..N with exactly one AAR, AMS and AS each;
  cv=none at i=1 and cv=pass above; the header block is well formed
  (no leading continuation line, every line has a colon); selectors are
  `[A-Za-z0-9._-]+`; a key record restricted to another service (`s=`
  without `email` or `*`) is refused (`t=s` has no meaning without `i=`);
- the top seal is by a trusted sealer, verifies (rsa-sha256 or
  ed25519-sha256) over the relaxed AAR, AMS and AS of instances 1..N with
  its b= emptied, and carries t=;
- the top AMS is by the same sealer and verifies over its h= headers,
  selected bottom-up as in DKIM;
- RECENCY: the seal's t= is no more than `imapTrustARCMaxAgeMinutes` before
  the server's INTERNALDATE (and at most 5 minutes after it), so an old
  seal cannot be replayed;
- DELIVERY: the message has exactly one To header holding EXACTLY ONE
  address (a group of one member reduces to it; an empty group has none),
  and it is one of `imapTrustARCRecipients`. A retailer's DKIM covers To,
  so a seal replayed from another subscriber's copy names that subscriber,
  and a co-recipient listed in To cannot replay a shared message. Accepted
  residual risk: a Bcc co-recipient of a genuine sealed message to this
  mailbox (invisible in the headers) could replay it with another body
  within the recency window -- retailer newsletters are sent one per
  subscriber;
- VERDICT: the top AAR comes from a trusted authserv-id and records
  `dkim=pass` with header.d (or header.i's domain) aligned with the
  sender's domain by the direct-DKIM rule; header.d / header.i must be
  unquoted and domain-shaped, and a property repeated within one result
  makes the AAR unparseable (a quoted value cannot smuggle a second
  header.i); when the result names the signature (header.b, a prefix of
  its b=; header.s), one of this message's DKIM-Signatures by that domain
  must match it (a header.b prefix must be at least 8 characters). This is
  a consistency check, not a security boundary.

Newest-first processing cannot let a replay override a genuine message:
a copy that fails authentication never reaches the parser, so it neither
suppresses nor replaces the genuine copy's products even with the same
Message-ID (tested); an authenticated later copy is genuine mail and may.

The message's own address in To for the promo sources is the operator's
Gmail address (the retailers mail it; Gmail auto-forwards to deals@):
real addresses live in gitops only. Locally the three captured stored
copies pass this whole path (Newegg and Total Wine via `arc`, wine.com via
`dkim`), and with Subject or To swapped they are refused.

Also from production: image.richrelevance.com serves JPEGs as
`Content-Type: image/jpg`; the declared `image/jpg` / `image/pjpeg` are now
read as `image/jpeg` (the sniffed type is still required to be an image).

## Observability

One log line per processed message: profile, a short hash of the
Message-ID (never the id or any URL), and counts of images seen, selected,
fetched, read, agreed and kept, with drop reasons. Metrics (bounded labels):

- `nagus_promo_messages_total{source,outcome}` processed | cached | deferred | disabled
- `nagus_promo_images_total{source,stage}` selected | fetched | read | agreed | kept
- `nagus_promo_drops_total{source,reason}` every drop reason above, plus the
  ingest-side `gate_refused`, `not_in_category`, `extract_failed`
- `nagus_promo_vision_calls_total{result}` ok | error
- `nagus_promo_vision_budget_exhausted_total{scope}` poll | day
- `nagus_promo_vision_calls_today`, `nagus_promo_vision_calls_per_day_limit`,
  `nagus_promo_vision_enabled`

Message outcomes are counted per poll (a cached message counts `cached` on
every re-poll); image stages and drops once per image per message; the
ingest-side drops once per product.

## Tests

Fixtures `internal/promo/testdata/{newegg,winecom,totalwine}.html` are the
three captured mails SCRUBBED (subscriber address, member number, preferred
store, every query string and token-like path segment replaced by
placeholders; HTML comments removed; structure kept). One TLS test server
plays every internet host (image CDNs, redirectors, product sites) through a
client that dials it for any name, so fixtures keep their real host names; a
fake OpenAI-compatible server answers per image (the image's first pixel is
its id) and per prompt. The end-to-end test runs the real imap connector
against an in-process IMAP server with DKIM-signed mail (and an unsigned
forgery, refused), the hdd ingester, the glovebox gate double and the
memory stores.

Live smoke (2026-10-16, LiteLLM `qwen3.6-35b-vllm-nothink`, the three saved
images, 1.6-2.5 s per read): both reads agreed on all three after the
prompt fix -- MSI Summit E13 Flip Evo 589.99 was 999.99, Acer Nitro V16
Gaming Laptop 1349.99 was 1699.99, Tolaini Al Passo Toscana 2022 27.97 was
29 (brand Tolaini); both laptops fail the hdd category fit, as intended.

## Deploy (gitops, not this repo)

Chart `vision.*` values render `NAGUS_VISION_BASE_URL`, `_MODEL` and the
budgets; `vision.apiKeyExternalSecret` syncs `NAGUS_VISION_API_KEY` from
Vault into its own Secret, referenced optionally (a missing key turns the
feature off, never blocks the pod). Egress: nagus has no NetworkPolicy
today; if one is added it must allow the image CDNs and redirectors listed in
the profiles.
