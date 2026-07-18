# ci-runner-alignment

## Brief

### TLDR

Full-surface audit of the melodic-software self-hosted CI system (ci-runner controller, provisioning, ci-workflows selector, standards runner-policy, github-iac routing governance) against GitHub's official documentation — fresh-eyes: every divergence re-derived from first principles, recorded rationale treated as evidence, never authority. Output is a prioritized report; then a joint one-by-one divergence walkthrough, urgent/highest-impact fixes first, GitHub issues filed for the remainder.

### Goal

Identify, with citations to official sources: (a) gaps where the build contradicts documented guidance, (b) divergences whose rationale does not survive first-principles re-derivation, (c) documented GitHub features/capabilities not utilized (each with an adopt/skip recommendation), and (d) alignment/integration improvements across the governing repos — so the fleet's design is either confirmed with live rationale or corrected.

### Constraints

- No code or config changes during the audit; discovery only.
- Pipeline: `/discovery:explore` (state inventory) then `/discovery:research-deep`, fanning out with **opus** agents.
- Doc-tier priority order: (1) self-hosted runner docs → (3) `actions/runner` repo + releases → (4) `actions/scaleset` client + ARC reference → (5) REST API (runners, scale sets, JIT) → (6) GitHub App auth/permissions → (2) security hardening last.
- Live-host checks on melo-desk-001 only; melo-lap-001 audited via repo state plus GitHub API runner/scale-set metadata.
- Completeness-driven, no token cap: coverage gate is every tier's primary sources read and every divergence re-derived — never a silently truncated sample.
- Reason-dont-recite: repo docs' self-justifications are input evidence; no divergence passes merely because it is documented.

### Acceptance criteria

- State inventory baseline exists: all five governing repos (ci-runner + releases, provisioning, ci-workflows, standards runner-policy component, github-iac routing governance), sampled downstream consumer usage, live desk state (controller, doctor output, Docker engine, image digests, versions), GitHub API runner metadata for both hosts.
- Every doc tier's primary sources read; each report claim carries a citation to an official source.
- Every detected divergence has a first-principles verdict: **confirmed** (live rationale survives), **unwarranted — fix**, or **needs-decision**, one entry per divergence.
- Unused-features section covers documented capabilities not in use, each with an adopt/skip recommendation and basis.
- Findings carry severity/priority sufficient for urgent-first triage.
- Report lands in `docs/topics/ci-runner-alignment/`.

### Captured assumptions

- "The docs" means official surfaces as of the audit date (2026-07-18); version comparisons are against latest releases at run time.
- Downstream consumer usage is sampled representatively, not exhaustively enumerated.
- Cross-repo findings file as issues in the owning repo at triage time, per ownership conventions.

### Out-of-scope

- Applying fixes or filing issues during the audit itself (post-walkthrough steps).
- Live verification on melo-lap-001 (deferred; trigger = drift suspected from repo/API evidence).
- Routing-policy changes.

### Deferred questions

- Report file layout within the topic slice (single report vs per-tier files) — arbiter: /architect.
- Which findings are fixed now vs filed as issues — arbiter: USER-RESERVED (one-by-one walkthrough decides).

## Plan

### Layout decision (Brief-deferred, arbiter /architect)

**Single file: `docs/topics/ci-runner-alignment/REPORT.md`.** Rationale:

- The Brief's triage criterion is severity-first across the whole surface; the highest-priority findings (D11, D12, D4) span different doc tiers, so per-tier files would fragment the walkthrough queue the report exists to feed.
- Per-tier depth already exists as the 13 research artifacts in `.work/ci-runner-alignment/`; per-tier report files would duplicate that layer with drift risk.
- Estimated volume (11 divergence entries + aligned findings + ~10 unused-feature entries) fits one navigable file. (The `runner-performance` topic offers no report-layout precedent — it holds only PLAN.md + design/ — so the fragmentation and duplication arguments above carry the decision.)

### Phase 1: Author REPORT.md [DONE]

Inputs: `.work/ci-runner-alignment/RESEARCH.md` (authoritative verdict ledger — do **not** re-adjudicate settled verdicts), the 13 research artifacts (citation sources), `EXPLORE.md` (state baseline + errata). Report must be self-contained: every claim cites an official URL copied from the research artifacts, never a `.work/` path.

Structure (heading contract: each divergence D2–D12 gets exactly one `### D<n>` heading — no combined headings; withdrawn D1 appears only in methodology prose, never as a `### D1` heading):

1. Headline synthesis — overall alignment posture, verdict counts.
2. Methodology + coverage — pipeline stages, state-inventory baseline summary (repos, live desk state, both-host API metadata — self-contained prose, no artifact paths), fetch date (2026-07-18), D1 phantom-withdrawal erratum, known not-covered remainders.
3. Triage index — one severity-ordered table over all findings (IMPORTANT → MEDIUM → LOW → SUGGESTION → confirmed/no-action), anchor-linked to entries; walkthrough queue order D11, D12, D4, then D5 and D6.
4. Per-divergence entries D2–D12 — verdict (Brief vocabulary), severity, first-principles rationale, official-source citations, and for needs-decision entries the decision options with trade-offs. ADOPT leans on needs-decision entries render as reserved recommendations ("recommendation, decision reserved for walkthrough"), never as chosen dispositions — the fix-vs-file call is USER-RESERVED.
5. Aligned/closed findings — compact section (observer App scope grant, API version pin, teardown semantics, etc.); claims here also carry official citations.
6. Unused features — each with adopt/skip/defer, basis (cited), and trigger where deferred.
7. Known limitations / observations appendix — carried unresolved items with no divergence home (installation-token rate ceilings unmeasured; out-of-scope claude-review job-failure observation).

If the critical-security no-grace clause is quoted verbatim, re-fetch its source URL first (two sources paraphrase differently; substance already confirmed).

**Sanity Check:** (run from repo root; `R=docs/topics/ci-runner-alignment/REPORT.md`)

- `test -f $R`
- `grep -cE '^### D(2|3|4|5|6|7|8|9|10|11|12)\b' $R` returns 11; `grep -c '^### D1\b' $R` returns 0
- every D-entry has a verdict and ≥1 citation: `awk '/^### D/{if(h&&(!v||!c))m++;h=$0;v=0;c=0} /Verdict/{v=1} /https:\/\//{c=1} END{if(h&&(!v||!c))m++;print m}' $R` returns 0
- no memory-tier refs: `grep -cE '\.work/|EXPLORE|RESEARCH-|RESEARCH\.md' $R` returns 0

### Phase 2: Acceptance-criteria verification + hygiene [DONE]

- Walk the Brief's six acceptance criteria one by one; record PASS/FAIL per criterion in this file (append below Plan). Scope per criterion: criteria 1 (state-inventory baseline exists) and the "every doc tier's primary sources read" half of criterion 2 are verified against the `.work/ci-runner-alignment/` artifacts (they are process facts, not report content); the remainder verify against REPORT.md itself, including citations in the aligned-findings and unused-features sections (manual scan — the awk check covers D-entries only).
- Markdown hygiene per repo tooling.

**Sanity Check:** `npx markdownlint-cli2 "docs/topics/ci-runner-alignment/**/*.md"` exits 0 (run from repo root); all six acceptance-criteria rows recorded PASS with per-criterion basis.

### Phase 3: Commit + PR [TODO] — user-gated

Branch `docs/ci-runner-alignment` off main; commit REPORT.md + PLAN.md + design-resolution.md (contract tier is branch — `docs/topics/` is tracked); PR per repo source-control conventions. **Gate:** confirm PR timing with the user — before or after the one-by-one walkthrough (walkthrough may amend the report with decisions).

**Sanity Check:** `git branch --show-current` returns `docs/ci-runner-alignment`; `git status --porcelain docs/topics/ci-runner-alignment/` clean after commit.

### Acceptance-criteria verification (Phase 2 record, 2026-07-18)

| # | Criterion | Result | Basis |
|---|---|---|---|
| 1 | State-inventory baseline exists | PASS | Verified against `.work/ci-runner-alignment/` stage-1 index + 7 sidecars: all five repos, sampled consumers, live desk state, API metadata both hosts |
| 2a | Every doc tier's primary sources read | PASS | Six tier artifacts in `.work/ci-runner-alignment/`, each with its own outcome-gate PASS and same-day fetches |
| 2b | Each report claim cites an official source | PASS | Per-entry URL check (awk) = 0 missing for D-entries; manual scan confirmed citations in aligned-findings and unused-features sections; all 29 distinct URLs verified present in research artifacts or live-verified (3 PR URLs, merged, titles match) |
| 3 | Every divergence has a first-principles verdict, one entry each | PASS | D-heading count grep (Phase 1 sanity check) = 11; D1 withdrawal recorded in methodology; verdict line present in every entry |
| 4 | Unused-features section with adopt/skip + basis | PASS | 14-row table, each with disposition + basis + trigger where deferred |
| 5 | Severity/priority per finding | PASS | Triage index orders IMPORTANT → SUGGESTION with walkthrough queue |
| 6 | Report lands in `docs/topics/ci-runner-alignment/` | PASS | `REPORT.md` present; markdownlint-cli2 exit 0 |

## Blast radius

LOW — docs-only artifact in a new topic directory; no code, config, or consumer-parsed surface changes; verdicts already adjudicated upstream in the research stage.

## Stress-test summary

Fresh-context plan-reviewer sub-agent ran (Step 3): 0 CRITICAL, 4 IMPORTANT, 6 SUGGESTION — all applied (sanity-check path/heading contract pinned, citation scope extended to non-D sections, acceptance-criteria verification scope reconciled with memory-tier artifacts, limitations appendix added, memory-tier ref guard broadened, ADOPT-lean reservation noted, layout precedent claim corrected). Formal /devils-advocate skipped: blast radius LOW, no triggers matched.

## Execution shape

Fully sequential — Phase 1 gates Phase 2 gates Phase 3; all main-session. Per-divergence fan-out rejected: entries need one consistent voice and strict ledger fidelity; volume is modest.

## Open questions

- PR timing relative to the user walkthrough (Phase 3 gate).
- Which findings are fixed vs filed as issues — USER-RESERVED, next stage.

## Handoff to implementation

### User-approval gates

- Phase 3 entirely (branch/commit/PR timing) — `[FALLBACK — confirm or override]`.
- Any report content that would pre-empt a USER-RESERVED decision (fix-vs-file) — present options only, never a chosen disposition.

### Execution shape ([EXEC-SHAPE] tagged)

- Single-file REPORT.md layout (Brief delegated this to /architect; rationale above).
- Sequential, main-session execution; no sub-agent fan-out for report authoring.

### Mechanical work

- Citations copied verbatim from research artifacts; verify each URL string exists in its source artifact before use (grep).
- Sequential fallback: n/a (already sequential).
