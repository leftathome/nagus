# CLAUDE.md -- operational guidance for agents working in nagus

## What this repo is

`nagus` is the acquisition/watch subsystem: monitor sources -> glovebox sanitize
-> extract/tokenize -> normalize -> store -> hard-filter -> enrich -> score ->
surface (push inbox+ping / pull `search_items`). See `README.md` and the design
doc at `docs/design/2026-07-01-nagus-design.md` (READ IT before changing
architecture). It is a sibling to glovebox (connectors + sanitization),
recognizer/archiver, and openclaw (agent runtime + delivery).

## Hard rules

- **Surface, don't act.** No auto-buy/bid/contact. The `search_items` endpoint is
  READ-ONLY (eyes, not hands). Any action path is separate and human-gated.
- **All listing content is untrusted.** It crosses the glovebox boundary before
  anything else. The extract stage emits a CONSTRAINED TYPED SCHEMA -- the worst a
  malicious listing can do is produce a wrong field value, never hijack. Never let
  raw listing free-text reach an LLM instruction context; quote it as data.
- **Never commit secrets.** API keys (Zillapi/Regrid/eBay/Keepa/...) live in Vault,
  synced to runtime. Not in git, not in config committed here.
- **Storage is an adapter.** SQLite+FTS5 and Postgres+pgvector both implement
  `internal/store.Store`. Do not couple the pipeline to one backend.

## Repo boundaries (file cross-repo work as the right project's bead)

- Connectors + sanitization + extract/tokenize -> **glovebox** repo.
- MCP tool registration, per-agent/audience access, delivery cron/inbox -> **openclaw** repo.
- The spine, item store, enrichment facets, scoring, category bundles, and the
  `search_items` MCP server -> here.

## Go / test discipline

- Go 1.26, modules. `go vet ./...`, `go test ./... -count=1 -race`, staticcheck
  (`.golangci.yml`) as the feedback loop. Write tests; run them; confirm behavior.
- Every commit needs a real, automatable test plan (no "I'll verify manually", no
  money-in-the-loop). The MemoryStore is the reference contract new store adapters
  must satisfy -- add adapters against the same tests.
- No emoji in code or strings (keep output ASCII-compatible).

## Issue tracking

Use `bd` (beads), not TodoWrite / markdown TODO lists. `bd ready` for next work,
`bd update <id> --claim`, `bd close <id>`. The v1 epic and tasks live in beads.

## CI

- **GitLab (`.gitlab-ci.yml`, gitlab.orac.local) is primary** for dev/test. The
  unit gate is vet + race tests; build/chart/release land with the first image
  (kaniko via `homelab/ci-templates`, as glovebox does).
- **GitHub Actions (`.github/workflows`) is the downstream public mirror + release.**
- Non-trivial changes: branch + MR (glovebox policy). Trivial fixes may push to main.


<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->
