# Plan: nagus v0.5.0, then quark QUARK-02..06, then joint releases

Created 2026-09-24 from the operator's direction: finish nagus-es1 and
nagus-w1p, cut ONE nagus release with the fixes BEFORE moving product-library
functionality to quark, then run every QUARK-* task to completion, then test
nagus + quark interoperability and functionality and cut releases of both.

## Steps

- [x] nagus-es1: removed the orphaned `nagus-land` Helm release (0 replicas,
      empty DB; backed up to the operator's Backups folder first). Uninstall
      run 2026-09-24 with the operator's explicit one-time approval; gitops
      19d3774 updates the comments that listed `nagus-land-data`.
- [x] nagus-w1p: MCP text block carries no listing values; fixed internal
      errors; strict arguments (MR !23, merged). openclaw's MCP bridge renders
      structuredContent, so agents see the same data.
- [x] nagus v0.5.0: CHANGELOG (89 commits since v0.4.0), chart 0.11.0 /
      appVersion 0.5.0. Deployed to prod first (gitops 4b68978) and
      smoke-tested through the openclaw gateway's network path, then tagged
      on 889371ac (mirror carried the tag to GitHub).
- [ ] nagus v0.5.1: the smoke test found search `limit` applied BEFORE
      ranking (nagus-cb8, present since July): fix, deploy, smoke, tag.
- [ ] QUARK-02 known-key text matching
- [ ] QUARK-03 catalog loaders
- [ ] QUARK-04 wine LWIN migration (nagus/internal/identity/lwin -> quark)
- [ ] QUARK-05 contribution path
- [ ] QUARK-06 cross-repo obligations
- [ ] Interop + functionality test of nagus and quark together
- [ ] Releases: nagus v0.6.0, quark (next)
