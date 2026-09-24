# Deals mailbox: email catalogs and flyers as a nagus source (2026-09-23)

## Decision (operator, 2026-09-23)

All catalog and flyer mailings go to `deals@totally.apocryph.al` (the
automation subdomain; a ForwardEmail alias with IMAP storage -- the hosted
provider the alerting receiver already uses, not an in-cluster mail server).

**nagus reads the mailbox; glovebox sanitizes.** nagus gets an `imap` source
type and runs every message's text through glovebox's `POST /v1/sanitize`
before extracting offers. Chosen over (a) glovebox reading the mailbox and
dropping files on a volume shared across namespaces, and (b) an agent reading
flyers with an LLM and writing offers into nagus (nagus exposes no write API
by design). This supersedes nagus-239's note that IMAP "belongs in glovebox".

It also makes nagus-9ib real: the sanitize client replaces
`sanitize.Passthrough` for EVERY source, not just email.

## Boundaries

- glovebox: the sanitize gate (classify, never rewrite; fail-closed).
- nagus: IMAP fetch, sender allowlist, per-sender structure parsing, offers.
- quark (later): release announcements arrive as hints on resolve.
- Operator: the ForwardEmail alias, subscriptions, and Vault seeding.

## Rules

- Only mail from an explicitly declared sender is read; everything else is
  ignored. One nagus source per sender, all on the same mailbox, each with its
  own `wineChannel`/`origin` (legality is per seller, not per mailbox).
- Stateless and idempotent, like the store connectors: each poll searches
  `FROM <sender> SINCE <lookback>`; the Message-ID is the source key.
- The mailbox never sends mail. No credentials in git.
- Fail-closed sanitize: `pass` keeps the original bytes; `quarantine` or any
  error drops the item. Dual-mode: with no gate URL configured, nagus keeps
  today's passthrough, so deploying the client changes nothing until the
  token is provisioned.

## Operator steps (seed Vault BEFORE any ExternalSecret lands)

1. ForwardEmail: create `deals@totally.apocryph.al` with IMAP enabled;
   generate its password.
2. `vault kv put secret/eso/nagus/deals-imap host=imap.forwardemail.net port=993 username=deals@totally.apocryph.al password=<...>`
3. `TOKEN=$(openssl rand -hex 32)`;
   `vault kv put secret/glovebox/ingest-tokens/nagus token="$TOKEN"`;
   `vault kv put secret/eso/nagus/glovebox-sanitize token="$TOKEN"`;
   reload glovebox (SIGHUP, per its vault-spec-13 runbook).
4. Subscribe `deals@totally.apocryph.al` to catalogs and flyers.

## Steps

- [x] nagus-9ib: generated glovebox sanitize client; fail-closed
      `listing.Sanitizer`; env-driven (`NAGUS_GLOVEBOX_SANITIZE_URL`,
      `NAGUS_GLOVEBOX_TOKEN`), passthrough when unset.
- [x] nagus-239: `imap` source type (sender filter, lookback, Message-ID key),
      tested against an in-process IMAP server. A message is accepted only
      when the TOPMOST Authentication-Results header (our MX's) reports a
      DKIM pass aligned to the sender's domain: a From header proves nothing
      on an open channel.
- [x] gitops: ExternalSecrets for the two Vault paths; nagus env; the
      sanitize URL (glovebox serves `/v1/sanitize` on port 9093). Gate live
      2026-09-23; hardened in chart 0.10.0 (nagus-lg7, nagus-4g3).
- [x] nagus-zsi (2026-09-24): ForwardEmail writes NO Authentication-Results
      header -- only ARC sets -- so the A-R check alone would have rejected
      every message. nagus now verifies DKIM signatures itself (go-msgauth;
      l= and rsa-sha1 refused), keeping the A-R path as the second mode.
      ARC is not trusted without verifying the seal chain.
- [x] Household forwards (nagus-zsi option 3): `imapForwarders` on a source.
      A message From a listed forwarder, DKIM-verified for the forwarder's
      domain, whose forwarded original names the source's sender, is that
      sender's mail (aspect `mail_forwarded_by`). Verified against a real
      Gmail hand-forward (real signature, real DNS) via the opt-in
      `TestRealCapturedMessage`; the message itself is never committed.
- [ ] Per-sender parsers, one at a time, each from a real captured email.
      - wine.com, first sample (2026-09-23 "More Champagne & Sparkling to
        love"): a RichRelevance personalized-recommendation mail. The six
        wines are images rendered at open time
        (image.richrelevance.com/rrmail/image/recs?...) behind tracked
        redirects; the mail carries NO names, prices or ids. Not parseable
        without fetching the tracked images/links (which reports the open
        and is exactly the act-on-mail-links behaviour we refuse). Its only
        concrete offer is store-wide (free shipping over $150, code, expiry).
        Needs a wine.com SALE/list mail as the parser sample.
      - Total Wine: no sample yet (filter forwarding only applies to new mail).
