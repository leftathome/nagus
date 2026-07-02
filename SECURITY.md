# Security policy

## Reporting

Report suspected vulnerabilities privately to the maintainer rather than opening a
public issue. Include reproduction steps and affected version/commit.

## Threat model notes

nagus ingests untrusted, potentially adversarial third-party listing content.
Core invariants:

- All external listing content crosses the glovebox sanitization boundary before
  any processing.
- The extract/tokenize stage emits a constrained typed schema; free text is
  carried as data and quoted, never as agent instructions (prompt-injection
  containment).
- The `search_items` interface is read-only. nagus performs no purchases, bids, or
  seller contact; such actions are out of scope and human-gated elsewhere.
- Third-party API credentials are provided at runtime from Vault and never
  committed to the repository.
