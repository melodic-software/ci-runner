# host-maintenance-defects

Five defect areas found while putting the local GitHub Actions fleet into gaming mode, plus an
ad-hoc artifact purge that had to become a repeatable capability.

## Where the work lives

Execution is tracked in GitHub issues, not here. This directory holds the plan and the design
record; the issues hold the current state.

| Issue | Area |
| :-- | :-- |
| [#196](https://github.com/melodic-software/ci-runner/issues/196) | Gaming-mode doctor probes shared one budget — **closed** by [#195](https://github.com/melodic-software/ci-runner/pull/195) |
| [#197](https://github.com/melodic-software/ci-runner/issues/197) | Artifact cap counts only catalog-visible bytes |
| [#198](https://github.com/melodic-software/ci-runner/issues/198) | Catalog drifts out of sync with artifacts on disk |
| [#199](https://github.com/melodic-software/ci-runner/issues/199) | No disk-vs-catalog audit, no reference-based purge |

Landed so far: [#194](https://github.com/melodic-software/ci-runner/pull/194) (retention wording),
[#195](https://github.com/melodic-software/ci-runner/pull/195) (per-probe deadlines),
[#200](https://github.com/melodic-software/ci-runner/pull/200) (`ActiveJob` tombstone filter),
[#201](https://github.com/melodic-software/ci-runner/pull/201) (orphan sweep without a catalog).

## Reading order

1. `PLAN.md` — the phases, their success criteria, and the decisions already settled.
2. `design/design.md` — the Stage 2 design gate.
3. `design/design-verification.md` — **read this before implementing any Stage 2 phase.** Two
   independent fresh-context reviewers verified the design against source; it overrides
   `design/design.md` wherever they conflict.

## Provenance

These files were written in a session memory slice and graduated here when Stage 2 was cut. They
cite exploration and research sidecars (`EXPLORE-*.md`, `RESEARCH-*.md`, `CONSENSUS-*.md`,
`VERIFY.md`, `PLAN-REVIEW.md`) that were working notes and were not graduated — every load-bearing
conclusion from them is restated in the three files above, with its own source citation. A citation
to a sidecar is a provenance note, not a pointer to a file in this repository.

## One process finding worth carrying forward

Phase 1.1 passed its plan, four review passes, and a full green test suite while still not reaching
its own success criterion. An independent reviewer found it in one pass. Every check reasoned about
the code as written; none asserted the end-to-end criterion the phase existed to reach.

The Stage 2 design then repeated the shape: each decision was verified against the code as it
exists, and three of them fail only when composed with the cap design they were written for.

Prefer the check that asserts the criterion over the check that inspects the mechanism.
