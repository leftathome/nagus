# Tasks live in beads, not here

This directory exists because Navigator creates it. **Do not put task documents
in it.**

Issue tracking for nagus is `bd` (beads) -- see the repo CLAUDE.md. Use
`bd ready` / `bd show <id>` / `bd update <id> --claim` / `bd close <id>`.
Beads state is canonical, survives compaction, and carries dependency edges that
a markdown file cannot.

Navigator's contribution here is `.agent/system/` (architecture) and
`.agent/sops/` (repeatable procedures), not work tracking.
