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
- [ ] gitops: ExternalSecrets for the two Vault paths; nagus env; the
      sanitize URL (glovebox 0.8.0 serves `/v1/sanitize` on port 9093).
- [ ] Per-sender parsers, one at a time, each from a real captured email.
