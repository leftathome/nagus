# Submitting a deal to nagus by email

nagus watches stores and feeds on its own. Some deals it can never see: a
price on a shelf, a tasting-room special, a newsletter whose products are only
pictures, a tip from a friend. For those, email the deal to the deals mailbox
and nagus treats it like any other listing: it is checked by the glovebox
sanitize gate, identified by quark, scored, and shown by `search_items` and
the watches.

Design and decisions: `docs/design/2026-09-26-deal-submission-jsonl.md`.

## When to use it

Use it for a wine or hard-drive deal that nagus cannot see itself:

- an in-store or tasting-room price;
- a retailer newsletter or flyer that is image-only;
- a tip: a deal someone mentioned, with a link.

Do NOT use it for items on a store nagus already watches (it polls those
itself; ask `search_items` first), for anything that is not a wine or a hard
drive with a price and an https link, or for private information (the note is
stored and other household agents can see it).

## Who can send

Only addresses on the allowlist configured by the operator (in gitops, never
in this repo), and the mail must pass DKIM for the sender's own domain. Mail
from anyone else is ignored and only counted. A household member may also
forward an allowlisted sender's submission by hand from a configured
forwarding address; it is credited to the original sender.

The address to send to is the deals mailbox. Ask Caspar, or read `mailbox`
from the MCP tool `deal_submission_spec`: it comes from configuration and is
not written down here on purpose.

## The format: one JSON object per line

Send a **plain-text** email. Every line that starts with `{` is one deal; all
other lines (greetings, notes, signatures) are ignored. HTML-only email is
refused as a whole: every mail client can send plain text.

Copy-paste examples (each is ONE line; do not let your mail client wrap it):

```
{"category":"wine","title":"Example Cellars Columbia Valley Syrah 2021","brand":"Example Cellars","vintage":2021,"bottle_ml":750,"price":"24.99","url":"https://shop.example.com/syrah-2021","seller":"Example Wine Shop","note":"in store only, ends Sunday"}
{"category":"hdd","title":"Example Digital 20TB SATA 7200rpm enterprise drive","brand":"Example Digital","mpn":"EX20T-0001","capacity_tb":20,"condition":"refurb","price":219.99,"url":"https://store.example.com/p/ex20t","seller":"Example Store"}
```

A whole message can look like this:

```
Two deals from today:

{"category":"wine","title":"Example Cellars Columbia Valley Syrah 2021","brand":"Example Cellars","vintage":2021,"price":"24.99","url":"https://shop.example.com/syrah-2021"}
{"category":"hdd","title":"Example Digital 20TB drive","brand":"Example Digital","mpn":"EX20T-0001","capacity_tb":20,"price":219.99,"url":"https://store.example.com/p/ex20t"}

Thanks!
```

### Fields (`nagus.deal/v1`)

| Field | Required | What it is |
|---|---|---|
| `category` | yes | `wine` or `hdd` |
| `title` | yes | what is for sale, as the store names it (up to 300 characters) |
| `price` | yes | major units: `"24.99"` or `24.99`; at most 2 decimals; no `$`, no commas |
| `url` | yes | an `https://` link to the deal or the store's product page |
| `currency` | no | ISO 4217 code, default `USD` |
| `seller` | no | the store or merchant |
| `brand` | no | wine: the producer/winery. hdd: the manufacturer |
| `mpn` | no | hdd only: manufacturer part number |
| `gtin` | no | UPC/EAN digits (8, 12, 13 or 14) |
| `vintage` | no | wine only: the year, as a number (`2021`, not `"2021"`) |
| `bottle_ml` | no | wine only: bottle size in ml (750, 1500) |
| `capacity_tb` | no | hdd only: capacity in TB |
| `condition` | no | hdd only: `new`, `refurb`, `used` or `parts` |
| `note` | no | free text, stored with the deal, never used to identify it |
| `schema` | no | if present, exactly `nagus.deal/v1` |

Any other field, or a known field spelled differently (`Title`), refuses that
line. `brand`, `mpn` and `gtin` are how nagus's catalog (quark) recognises a
drive; for wine, the producer in `brand` plus the title is how it recognises
the wine, so give the producer whenever you know it.

The machine-readable schema is served by nagus at `GET /schemas/deal/v1.json`
(in-cluster: `http://nagus.nagus.svc.cluster.local:8080/schemas/deal/v1.json`)
and is committed at `internal/deal/deal-v1.schema.json`.

## Limits and rules

- At most **50 deal lines per message**; lines past the 50th are refused.
- At most **4096 bytes per line**.
- Each line is judged on its own: one bad line never sinks the others.
- Reading stops at your signature (`-- `), at a forwarded or
  "Original Message" marker, or at an "On ... wrote:" line, so a reply that
  quotes an earlier submission does not submit it again.
- nagus never marks the mailbox read and never deletes mail. Re-reading a
  message never duplicates a deal. Sending the same deal in a NEW message is a
  new submission.
- A deal stays live while its email is inside the lookback window (14 days
  by default), then drops off the surface.

## Checking what happened

Submissions are processed on the deal sources' next poll (minutes). Then:

- **Ask Caspar**, or
- call the MCP tool **`deal_submission_status`**:
  - with `message_id` (the Message-ID of the email you sent): per line,
    `accepted` with an `offer_id` (usable with `get_item`) and quark's
    `product_id` once resolved, `rejected` with a reason code, or `pending`;
  - with `principal` (your sender name, or your address): your latest
    messages' outcomes and reason codes only.

Status never repeats what you wrote, only line numbers and codes. Common
reason codes:

| Code | Meaning |
|---|---|
| `bad_json` | not one valid JSON object on the line (often: the line was wrapped) |
| `unknown_field` | a field the spec does not have, or wrong capitalisation |
| `missing_field` | category, title, price or url missing |
| `bad_type` | e.g. `"vintage":"2021"` instead of `2021` |
| `bad_price` | not a plain positive amount with at most 2 decimals |
| `bad_url` | not `https://` with a host |
| `field_not_for_category` | e.g. `vintage` on an hdd line |
| `category_not_enabled` | that category is not accepted here yet |
| `gate_refused` | the glovebox sanitize gate refused the text |
| `not_in_category` | nagus decided it is not a wine / not a hard drive (an SSD, merchandise) |
| `too_many_lines`, `line_too_long` | over the limits above |
| `gate_unavailable` (pending) | the gate was down; retried automatically |

The full list is in `deal_submission_spec` (`reason_codes`).
