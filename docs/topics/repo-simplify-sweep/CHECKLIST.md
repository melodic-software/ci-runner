# batch-simplify checklist — repo mode

Run: `/code-tidying:batch-simplify` (repo scope), session started 2026-08-30.
Branch: `claude/repo-code-tidying-simplify-z75kiv`.

## Phases

- [x] Phase 0 (repo mode): Precondition — no tracked modifications in the sweep universe; run state initialized under `docs/topics/repo-simplify-sweep/`
- [x] Phase 1: Discover files — `git ls-files --cached --others --exclude-standard` from repo root (244 files)
- [x] Phase 2: Filter to code files — 34 excluded (markdown, CI enforcement config, agent config, synced bootstrap, go.sum, release data records, fixtures); 210 survive
- [x] Phase 3: Verify existence — all 210 exist in the working tree
- [x] Phase 4: Group files — deterministic base pass (21 groups) + agent refinement (20 groups, 5 waves; file-set identity independently verified)
- [x] Phase 4.5 (repo mode): Confirmation gate — inventory summary recorded; unattended authorization recorded for Phase 8
- [x] Phase 5: Create tasks — one per group (24 tasks tracked)
- [x] Phase 6: Run simplification waves — 20 groups across 5 waves
- [x] Phase 6.1 (repo mode): Refutation verifier per group — 19 NOT REFUTED, 1 REFUTED (G3, reverted + safe-subset re-run)
- [x] Phase 6.2 (repo mode): Deliver each wave — 5 wave commits pushed to the designated branch (no PRs — session harness constraint)
- [x] Phase 6.5: Capture deferred items — consolidated; High filed as issue #313; rest persisted in RUN-STATE.md
- [x] Phase 7: Final cross-ecosystem verification (union pass) — PASS; unmapped lanes reported as unmapped
- [x] Phase 8: Summary report — REPORT.md

## Notes

- Delivery deviation: repo-mode default is one PR per wave; this session's harness
  mandates all work land on `claude/repo-code-tidying-simplify-z75kiv` with no PRs
  unless explicitly requested. Waves are delivered as sequential commits on that
  branch instead.
- Filing tier: High only (repo-mode rule). All tiers persist to RUN-STATE.md.
