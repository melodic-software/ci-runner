# batch-simplify checklist — repo mode

Run: `/code-tidying:batch-simplify` (repo scope), session started 2026-08-30.
Branch: `claude/repo-code-tidying-simplify-z75kiv`.

## Phases

- [x] Phase 0 (repo mode): Precondition — no tracked modifications in the sweep universe; run state initialized under `docs/topics/repo-simplify-sweep/`
- [x] Phase 1: Discover files — `git ls-files --cached --others --exclude-standard` from repo root (244 files)
- [x] Phase 2: Filter to code files — 34 excluded (markdown, CI enforcement config, agent config, synced bootstrap, go.sum, release data records, fixtures); 210 survive
- [x] Phase 3: Verify existence — all 210 exist in the working tree
- [ ] Phase 4: Group files — deterministic base pass (21 groups) + agent refinement
- [ ] Phase 4.5 (repo mode): Confirmation gate — inventory summary; unattended authorization recorded for Phase 8
- [ ] Phase 5: Create tasks — one per group
- [ ] Phase 6: Run simplification waves
- [ ] Phase 6.1 (repo mode): Refutation verifier per group
- [ ] Phase 6.2 (repo mode): Deliver each wave (commit + push to the designated branch; no PRs — session harness constraint)
- [ ] Phase 6.5: Capture deferred items
- [ ] Phase 7: Final cross-ecosystem verification (union pass)
- [ ] Phase 8: Summary report

## Notes

- Delivery deviation: repo-mode default is one PR per wave; this session's harness
  mandates all work land on `claude/repo-code-tidying-simplify-z75kiv` with no PRs
  unless explicitly requested. Waves are delivered as sequential commits on that
  branch instead.
- Filing tier: High only (repo-mode rule). All tiers persist to RUN-STATE.md.
