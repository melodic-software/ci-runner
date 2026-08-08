# PLAN — host-maintenance-defects (revision 4)

**Status at graduation.** Stage 1 phases 1.1 and 1.2 have landed (#195, #194); 1.3 is parked. Stage 2
has run its design gate and shipped two independent fixes found by that gate's own adversarial review
(#200, #201). Its three phases are otherwise open and tracked as #197, #198, and #199 — see
`README.md`, which is the current-state pointer. This file is the plan of record, not a status board.

Revision 4 corrects success criterion 3 and Phase 1.1's gating scope against the shipped
implementation (`055c196`), and hands the three Stage 2 phases to `design/design.md`, which supersedes this
file for those phases.

**Process finding worth more than any single correction here.** Phase 1.1 passed this plan, four
review passes, and a full local green suite while still not reaching its own success criterion 3. An
independent reviewer on the PR found it in one pass. The gap was structural: every check in this
directory reasoned about the code as written, and none asserted the end-to-end criterion the phase
existed to reach. Stage 2's Sanity Checks should be read with that in mind — prefer the check that
asserts the criterion over the check that inspects the mechanism. The header read "revision 2" while the body carried revision 3's
decisions; both are now revision 4.

Draft, pending approval. Memory slice; graduates to `docs/topics/host-maintenance-defects/PLAN.md`
on the `ci-runner` task branch when Stage 2 is cut.

Inputs, all in this directory: `EXPLORE.md` + 6 sidecars, `RESEARCH.md` + 3 sidecars,
`RESEARCH-automode-authoring.md`, `VERIFY.md`, `PLAN-REVIEW.md`. On conflict, `PLAN-REVIEW.md`
overrides `VERIFY.md` overrides the EXPLORE/RESEARCH sidecars.

Revision 2 rewrites the former Phase 2.1, reorders Stage 2, and adds phase-gating to Phase 1.1, per
three approved decisions. It also closes the remaining `PLAN-REVIEW.md` findings; the mapping is in
the Review-closure table at the end.

## Brief

Five defect areas found while turning the local ephemeral GitHub Actions fleet off for gaming mode on
`melo-desk-001`, plus the ad-hoc remediation that must become repeatable.

**"Thread" is not a term of art here** — it was session shorthand, and `PLAN-REVIEW.md` 23b is right
that it appears in no artifact. The work is the phases below; no thread numbering survives.

Success criteria:

1. Disk usage of worker artifacts cannot exceed the configured cap while unreferenced files sit
   inside the retention window — **without any file a live worker depends on becoming evictable**.
2. The catalog cannot silently shed records whose files survive, and a tombstoned record cannot
   silently swallow later patches.
3. `host doctor` **exits 0** in gaming mode, and the Docker Desktop check surfaces the stopped engine
   as WARN rather than FAIL. The `host-inventory/*` rows are WARN in gaming mode too — revision 3 said
   they would stay FAIL, which contradicts the exit-0 half of this criterion, because `doctor.go:182`
   returns `ExitDegraded` for any check that is unhealthy and not `Advisory` or `Skipped`. Both halves
   are reachable only together.
4. Both the disk-vs-catalog audit **and** the reference-based purge are first-class capabilities, and
   host maintenance is documented.
5. Read-only `gh` is not blocked by the auto-mode classifier, while `gh` writes against repositories
   outside the named set stay gated.

Out of scope: an install-root version prune (`docs/releases.md:137`'s floor reading makes nine
directories conformant), and any change to `classifyAllShell`.

## Staging

**Stage 1** — Phases 1.1, 1.2, 1.3. Independent; unblock daily friction.
**Stage 2** — Phases 2.1, 2.2, 2.3, in that order. Shared files (`artifacts.go`, `file_store.go`,
`logs.go`).

**Stage 2 order is audit-first, and that is load-bearing.** The audit is read-only and lands before
either change to deletion behaviour, so it is the instrument that verifies them and lets the operator
see which of the measured 2239 MiB is genuinely unreferenced *before* anything can act on it. It also
supplies the only satisfiable form of Phase 2.2's check.

**Design gate:** `/planning:design`, light, scoped to **all three Stage 2 phases** when Stage 2 is
cut — not 2.1 and 2.3 only. Phase 2.2 carries the livelock and the `Merge`-signalling blast radius
and is the phase that most needs it. Stage 1 is Tier C (localized fix, config, docs) and early-exits
the gate.

---

## Stage 1

### Phase 1.1: Per-probe deadlines, and phase-gate the Docker probes [TODO]

`ci-runner`. Two defects, and **both are required** — per-probe deadlines alone cannot reach success
criterion 3.

Defect A — shared budget. `GamingManager.Inventory` runs up to four probes and `Verify` runs three,
each on ONE context from `localProbeContext` (`internal/app/app.go:605-606`, driven by
`controller.localProbeTimeout: 15s`). `docker desktop status --format json` costs 16.2 s measured
when Desktop is stopped, so it consumes the whole budget and later probes fail instantly on an
expired context.

Defect B — the Desktop probe cannot succeed at all in the target state. At 16.2 s against a 15 s
budget it expires regardless of how the budget is divided; `gaming.go:21-24` then sets
`DesktopStatusUnknown` and `doctor.go:119` (`Healthy: inventory.DesktopStatus != "unknown"`)
evaluates FAIL. Per-probe deadlines fix probes 2-4 and cannot fix probe 1.

- `internal/host/gaming.go` — each probe in `Inventory` (`:18-58`) and `Verify` (`:74-110`) gets its
  own derived deadline under the caller's parent budget. Per `RESEARCH-go-per-probe-deadlines.md` a
  derived child's deadline is "no later than" the parent's, so the parent still bounds the whole.
- Distinguish "did not complete" from "returned a negative observation" via
  `context.WithTimeoutCause` + `context.Cause` — one `DeadlineExceeded` sentinel serves every
  deadline and cannot alone separate a child timeout from a parent cancellation.
- **Mark the Docker Desktop check `Advisory` in gaming mode — not `Skipped`.** `DoctorCheck.Advisory`
  (`doctor.go:21-24`) is documented for exactly this case: *"marks a check whose unhealthy result is
  expected operational state to surface, not a fault: it renders as WARN and never degrades the
  doctor exit code."* Rendering is `Skipped`→SKIP, `!Healthy && Advisory`→WARN, `!Healthy`→FAIL
  (`:159-166`), and the exit gate excludes both (`:172`). `Advisory` preserves the observation where
  `Skipped` discards it, and the in-tree precedent is `pending-os-reboot`
  (`doctor_inspector.go:175`) — the WARN already seen on this host. Doctor already branches on
  `desired.Mode == model.ModeGaming` at `doctor.go:123`, so `docker-desktop-cli` (`:119`) is gateable
  there.
- **Scope: `docker-desktop-cli` AND the `host-inventory/*` rows, both `Advisory` in gaming mode.**
  Revision 3 narrowed this to `docker-desktop-cli` only, on the strength of `CONSENSUS-fresh.md` F4.
  F4 remains correct about what it actually established — `GamingInventory.Problems` is a flat
  `[]string` with no provenance (`gaming.go:24` formats Desktop, engine, container, and WSL problems
  identically), so **attributing** a row to a specific probe is not implementable as scoped. But
  attribution was never needed: in gaming mode every one of those probes is expected to fail, so the
  whole set is advisory together and no per-row provenance is required. The narrowing also made
  criterion 3 self-contradictory — a FAIL row is a non-`Advisory` unhealthy check and
  `doctor.go:182` turns it into `ExitDegraded`. The shipped implementation (`doctor.go:126,130`)
  applies `Advisory: gamingMode` to both, which is the only shape that reaches exit 0.
  Accepted cost: a genuine non-gaming-related probe failure reads WARN instead of FAIL while the host
  is in gaming mode. `Advisory`'s documented purpose is to preserve exactly that observation without
  degrading the exit code, and the rows still read FAIL in every other mode because `gamingMode`
  gates them.
- `internal/app/doctor.go:131` — `gaming-postconditions` (`Healthy: err == nil &&
  verification.DesktopStopped && …`) is where the three-state rendering belongs; `desktopStopped=false`
  currently means "could not verify" and reads as "observed still running".
- `internal/app/app.go:294` — `host game` passes the RAW command context to `Inventory`. Wrap it, or
  the fix never reaches the command that creates the condition.

- **A separate desktop budget is required, and revision 3 was wrong to exclude it.** Revision 3 listed
  "raising `localProbeTimeout`" under Not doing, calling it symptom-only. That is right about raising
  the *shared* budget and wrong about the conclusion drawn from it: dividing one 15s budget evenly
  still cannot fit a ~16s probe, so per-probe deadlines alone leave the Desktop probe expiring every
  time, `Verify` reporting `DesktopUnverified`, and `gaming-postconditions` — which had no advisory
  branch — returning `ExitDegraded` on exactly the healthy state this phase targets. Found by the
  Codex reviewer on PR #195 (P1), not by this plan or its four review passes. `DesktopProbeTimeout`
  budgets that probe alone; it is optional and falls back to `LocalProbeTimeout`.
- **`gaming-postconditions` needed the `Advisory` treatment too.** The phase gated
  `docker-desktop-cli` and the `host-inventory/*` rows and stopped there, leaving a third
  non-advisory check on the same path. It is now advisory when a postcondition could not be
  **checked**, and still FAIL when one was checked and found unsatisfied.
- **Every probe error marks its postcondition unverified, not just a timeout.** Revision 3 keyed
  `DesktopUnverified` off `ErrProbeTimedOut` alone, so an executable-resolution, launch, parse, or
  parent-cancellation failure rendered as `false` — an unchecked postcondition classified as an
  observed one. Codex P2. No caller needed the timeout provenance, so the per-probe `*TimedOut`
  locals are gone rather than moved to a second field.

Not doing: the named-pipe fast-negative (Docker does not document that pipe as stable public
interface), and raising the *shared* `localProbeTimeout` (that would slow every probe to fit the one
that is slow; the separate desktop budget is the targeted form).

Tests. `internal/host/gaming_test.go` and a `DesktopProcess` fake **do not exist yet** — both are new,
with no in-package precedent (`internal/host` has `host_test.go`, `json_log_test.go`, three platform
tests; the only `DesktopProcess` implementors are production types). Match the repo style: table-free,
one behavior per test, assertion-shaped names, hand-written fakes, no mocking framework.

**Sanity Check:**

- `go test ./internal/host ./internal/app` exits 0.
- A test with a fake `DesktopProcess` whose `Status` blocks past its own deadline asserts the other
  three probes still run AND still report their real values — the property, asserted directly rather
  than proxied by a `grep -c` count, which a single shared `probeWithDeadline` helper would fail while
  being the better implementation.
- `git grep -n '\.Inventory(ctx)' internal/app/app.go` returns nothing.
- On the live host in gaming mode, `ci-runner host doctor` exits 0 (not `ExitDegraded`) and
  `docker-desktop-cli` reports `[WARN]`. **Not** "zero `[FAIL]` rows": that asserts a property of the
  whole doctor output including checks this phase does not touch. The exit code is the criterion; a hardcoded `[SKIP]`
  expectation would also fail a correct `Advisory` implementation, repeating the over-specified-proxy
  defect of `PLAN-REVIEW.md` 8.
- `golangci-lint run` exits 0 (`nolintlint`: `allow-unused: false`, `require-explanation: true`,
  `require-specific: true`), and the `comment-hygiene` and `go-windows-build` CI jobs pass —
  `gaming.go` is Windows-relevant.

**Rollback:** revert the commit. No persistent state is written by this phase.

### Phase 1.2: Correct the retention wording toward the floor [TODO]

`ci-runner`. `README.md:409` "retains the latest three known-good pairs" reads as a cap;
`docs/releases.md:137` "retain **at least** the latest three known-good compatibility pairs" is
explicitly a floor. Repo-wide grep confirms no third statement.

- `README.md:409` — align with the policy document, preferring a pointer to `docs/releases.md`'s
  freshness-policy section over restating the number.

**Sanity Check:**

- `grep -n 'retains the latest three known-good pairs' README.md` returns nothing — the exact
  superseded string is gone. (The previous formulation required a human to judge "unqualified".)
- `lychee --offline` passes if a link is added.

**Rollback:** revert the commit.

### Phase 1.3: Permission surface — three layers [TODO]

`dotfiles`. **Blocked on the operator prerequisites.** Design per `RESEARCH-automode-authoring.md`
§B3, which the review confirmed is a faithful, non-overreaching read resting on B3's safe derivation
route.

Load-bearing finding: exclusion language inside an `autoMode.allow` entry is **inert as a block** — an
allow entry's only documented power is to override matching `soft_deny` rules as exceptions, and an
exclusion clause creates no `soft_deny`. The existing entry's "limited to this CLOSED list and nothing
else" therefore gates nothing today.

1. **`permissions.deny`** — enumerable destructive `gh` subcommands with no read-only form under the
   same prefix. Added under `claudeSettings.force.permissions.deny` in `.chezmoidata/claude.json`
   (`claude-permissions.json` is upstream-owned by `melodic-software/standards`). The review verified
   this placement composes correctly: `modify_settings.json:28` force-merges into `$cfg`, and the
   floor union at `:41-47` reads the already-merged value.
2. **A custom `autoMode.soft_deny` entry** — the class boundary in prose, carrying the
   `[named+specifics]` tag so gated ≠ forbidden, plus an explicit non-human-turn clause, because that
   escape hatch is unavailable to subagents and scheduled fires.
3. **`autoMode.allow`** — read-only `gh`, purely permissive, no exclusion language.
4. **Amend the existing `gh` write entry** to scope itself to writes, rather than appending a second
   entry that contradicts it. **Reconcile against source `autoMode.allow[13]`** (VERIFY R6) — an
   undeployed entry whose entire subject is the `gh api` read/write split this phase legislates, and
   which the same apply deploys.
5. **Add a content disclaimer** to the amended write entry and the commit/push grant. From
   `critique` run 6, its highest-impact finding: those entries bless verbs but never disclaim content,
   so a classifier can read them as clearing `Excess Sensitive Detail`, `Credential Leakage`,
   `Sensitive-Source Provenance`, `PII Data Handling`, and `Out-of-Place Publication` for body text —
   on repositories this machine's own `environment` entry marks public. Copy the shipped
   `Git Push Destination` pattern: blesses verb, destination, and argument shape only; every content
   rule and the HARD Data Exfiltration rule judges the action as if the entry did not exist. The
   read-only grant publishes no body text, so the disclaimer belongs on the write side.
6. **Author no condition the classifier cannot evaluate.** From a second `critique` run, its most
   serious finding: existing grants carry structurally unverifiable conditions (push destination
   "canonicalizes to a repository under those two owners", "no URL-rewrite rule matching its
   destination", whether an `add` pathspec names a directory) and then say "where that cannot be
   established, judged on its own terms" — so the grant **self-nullifies**. The read-only `gh` grant
   must be decidable from the command text alone: no scoping by repository ownership, remote
   configuration, or working-tree state. The write-side gate goes in `soft_deny`, where an
   unverifiable condition fails toward blocking rather than toward silence.
7. **Fix `autoMode.allow[5]`'s pre-classifier sentence.** It reads verbatim "with classifyAllShell
   enabled in this same block **the pre-classifier rules do not resolve at all**", which
   `RESEARCH-gate-semantics.md` §A3 establishes is false as written: the documentation supports the
   **allow-only** reading, and deny/ask resolve before the classifier in every permission mode.
   The sentence sits in tracked fleet config and argues against the very `permissions.deny` layer
   item 1 adds — and two independent reviewers read it exactly that way. Narrow the phrase to
   `permissions.allow`; the entry's actual argument survives intact.

Form constraint from both runs: entries are "long enough, and cross-referenced tightly enough, that a
classifier reading them once under time pressure will apply the headline grant and miss a buried
carve-out." Keep the new entry short; do not lengthen the amended ones beyond the disclaimer.

Not doing: the CLAUDE.md leg. Docs place project conventions in CLAUDE.md and cross-project rules in
`autoMode`; this posture is cross-project. It buys no enforcement, and authoring it trips the
machine's own `Permission Surface Edits` soft_deny by its own stated purpose.

**Layer 1 is real enforcement, now grounded.** `RESEARCH-gate-semantics.md` §A settles what was
DERIVED-not-stated: `classifyAllShell` is scoped to *allow* rules in both of its documentation
surfaces, and nothing documents deny or ask as suspendable. `PLAN-REVIEW.md`'s "could not verify" on
this point and `CONSENSUS-fresh.md` F7's contradiction are both resolved in the plan's favour.

**This phase may not work, and that is stated up front.** A shipped `Read-Only Operations` allow entry
was already present at index 4 when `gh issue list` was observed blocked, so layer 3 is the same shape
as a rule that already existed and already failed (`PLAN-REVIEW.md` 9). Item 6 is the best available
mechanism for why — an unverifiable condition self-nullifying — but no introspection exists to confirm
which rule fired.

**Remediation branch, corrected.** Revision 2 said to escalate to a `PreToolUse` hook.
`RESEARCH-gate-semantics.md` §B removes that: whether a hook returning `"allow"` bypasses the
auto-mode *classifier* — a different gate from permission rules — is **NOT DOCUMENTED**, the docs
state a general "hooks can tighten restrictions but not loosen them past what permission rules allow",
and the one documented hook-widening recipe is **inoperative under this machine's config**, because it
depends on a broad `Bash` allow rule that auto mode drops and `classifyAllShell` drops again. The
correct statement is that **there is no documented escape hatch above `autoMode.allow`**: a failing
entry is rewritten, not escalated around. If rewriting does not clear it, raise it upstream —
`claude-code-plugins#787` is the existing vehicle and is still open.

**A premise check that weakens this phase's urgency.** While answering question C, the researcher ran
`gh issue view 787 --repo melodic-software/claude-code-plugins` on this host at 2.1.225 **and it was
not refused** — nor were seven `curl` doc fetches, a `git show` against the dotfiles remote, or any
`jq` read. Read-only `gh` is therefore **not categorically blocked** here. That does not disprove the
original `gh issue list` denial — different subcommand, different arguments, different session, and
the classifier scores each action on an internal severity scale rather than matching a pattern, so two
invocations of one command family can land differently. But it does mean this phase addresses an
intermittent, unreproduced denial rather than a standing block, and that should inform how much is
spent on it.

**Re-verify the template analysis before authoring.** This phase's composition analysis was done
against `dotfiles` `origin/main` @ `d19ae8c`. `9265154 fix(claude): grant the worktree root, retire
the loop-worktree root (#410)` has since changed `dot_claude/modify_settings.json` by 45 lines — the
exact template the layer-1 placement reasoning depends on. Redo that check at `9265154` or later
before writing any entry.

**Operator decision, 2026-08-08: this phase is PARKED.** Read-only `gh` is not categorically blocked
on this host (a subagent ran `gh issue view` unrefused at 2.1.225), so the phase addresses an
intermittent, unreproduced denial; and #410 moved the template underneath its analysis. Unpark when
the block recurs and is reproducible.

Constraints: `autoMode.*` is read only from user/managed/`--settings` scope. Append alongside
`"$defaults"` — omitting the sentinel replaces the entire built-in list for that section. Work in a
sibling worktree at `../chezmoi-worktrees/<name>`, because uncommitted `.chezmoidata` edits leak into
live `chezmoi data`/`apply`.

**Sanity Check:**

- `pwsh dotfiles: tools/claude-settings-compose/test.ps1` exits 0. This is the correct harness and it
  says so itself: "`chezmoi execute-template` cannot exercise it — it does not populate
  `.chezmoi.stdin` — so every case here drives the REAL template through `chezmoi cat` against an
  isolated throwaway destination directory."
- After the **second** operator apply (see prerequisites): `claude auto-mode config` shows
  `"$defaults"` still expanded in place and the new entries present.
- `gh issue list --state open --limit 5` runs without a classifier block — or the remediation branch
  above fires.

**Rollback:** `claude auto-mode reset` (v2.1.212+) removes the `autoMode` section from
`~/.claude/settings.json` only; the chezmoi source change reverts by PR revert plus a re-apply.

---

## Stage 2 — audit first

**`design/design.md` overrides this section.** The design gate declared in §Staging has run. It decomposes
the "identity signal" question into three defect classes with three answers (D1 adoption-derived
`referenced`, D2 temp filename filter on the cap lane only, D3 per-path `referenced` population),
specifies the drop-journal bound D4 left open, and supersedes revision 3's `indexAdopted`
re-open proposal via D5. Read `design/design.md` before implementing any phase below.

### Phase 2.1: First-class audit and reference-based purge [TODO]

`ci-runner`. Lands **before** any change to deletion behaviour. `host logs --cleanup` cannot reach
orphans newer than the retention cutoff, has no dry-run, returns bare `error`, and discards the
`referenced` set and `total` it already computes.

- **Two new modes**, not one — success criterion 4 names both the audit and the purge:
  - a read-only audit reporting per-directory byte accounting and per-file classification:
    referenced-and-present, referenced-and-missing, unreferenced-and-past-cutoff,
    unreferenced-and-within-cutoff;
  - a reference-based purge deleting unreferenced files the audit classifies, **excluding in-flight
    temporaries** (`.ci-runner-log-*.tmp`, `.ci-runner-diag-*.tmp`, `.ci-runner-resources-*.tmp` —
    `artifacts.go:111,146,228`), under explicit confirmation, and **retaining** the Docker inventory
    precondition, because it deletes.
- The read-only audit **does not require the Docker inventory precondition** and states in its output
  whether the inventory was available. The precondition guards deletion of a live worker's artifacts;
  a mode that deletes nothing cannot hit that hazard, and dropping it is what makes the tool usable in
  gaming mode and during a disk-pressure incident. `[FALLBACK — confirm or override]`
- A result type where `LogCleaner.Cleanup` and `FileArtifactSink.cleanup` return bare `error`.
- `internal/app/logs.go:29` — the exclusivity error string is a hardcoded literal naming the three
  current flags and must name all five, or the message lies.
- `internal/app/app.go:597` usage text omits `--cleanup`; add it and both new modes.
- Runbook: a new host-agnostic `provisioning: runbooks/ci-runner-artifact-retention.md`, matching the
  existing `ci-runner-<procedure>.md` naming and avoiding the two-host duplication. **Branch from
  `provisioning` `origin/main` (`8781b10`)** — the worktree is at a DETACHED HEAD (`da5192f`) and the
  in-scope paths are identical, so branching from `origin/main` loses nothing.

**Sanity Check:**

- `ci-runner host logs --<audit-flag>` with Docker stopped exits 0 and reports counts.
- `git grep -n 'mutually exclusive' internal/app/logs.go` shows all five flags named.
- `go test ./internal/app ./internal/runtime/docker` exits 0; `golangci-lint run` exits 0.
- The `machine-specific-paths` CI job passes — the new runbook's subject is a named host with
  drive-letter paths, which is exactly what that gate catches.

**Rollback:** revert. Read-only until the purge mode is invoked; the purge is confirmation-gated.

### Phase 2.2: Close the two catalog-drift paths [TODO]

`ci-runner`. Lands second so the audit can observe it.

- `internal/jobindex/file_store.go:359-397` — `compactOldestCompleted` drops terminal records under
  save-cap pressure (`encodeWithinCapacity:310`, `maximumJobState = 8<<20`) with no tombstone and no
  file deletion, so files outlive their records.
  **Do not convert the drop into a tombstone.** A tombstone *grows* the encoded record while the whole
  path exists to shrink it below `maximumJobState`, and tier 1 (`compactOldestTombstones`) has already
  exhausted the tombstones by the time tier 2 runs — the livelock the code's own comment at `:300-302`
  warns about.

  **The drop sink, specified.** Revision 2 said "record the drop where the audit can surface it" and
  named no mechanism; `CONSENSUS-fresh.md` F3 showed the plan's own text foreclosed both candidate
  sinks. Resolution: the sink is **a structured record written by the CALLER of
  `encodeWithinCapacity`**, not by `compactOldestCompleted` and not inside `*Catalog`.
  `compactOldestCompleted` stays a pure function over `*Catalog` and returns the identities it
  dropped; its caller already sits in the save path and does have filesystem access, and appends them
  to a bounded, self-pruning drop journal beside the state file. This clears the livelock (nothing
  grows inside the structure being shrunk below `maximumJobState`), clears the layering objection (no
  filesystem work under `*Catalog`), and preserves the per-record identity the Sanity Check demands.
  **The rollback line changes accordingly:** this IS an on-disk addition, so rollback is revert plus
  removing the journal, and the journal must be bounded or it becomes the next unbounded-growth
  defect.
- `internal/jobindex/catalog.go:82-84` — `Merge` returns a tombstoned record unchanged with `nil`
  error, silently discarding later patches. All 8 production call sites discard the record
  (`_, err :=`).
  **Do not signal it as an error.** `AdoptAndCleanup` returns on `indexAdopted` error
  (`artifacts.go:301-304`), so turning a tombstoned-key adoption into a hard failure would abort
  cleanup *before* any runs — a disk-pressure amplifier, not a fix.

  **What acts on the signal, specified.** Revision 2 stopped at "signal the no-op out-of-band", which
  both reviewers correctly called detection with no remediation — `indexAdopted` discards the record
  at `artifacts.go:350`, so nothing consumed it and finding 14 stayed open. Resolution:
  **`indexAdopted` consumes the signal and re-opens the collision.** On a tombstoned-key hit it must
  not silently proceed; it records the adopted worker under a key that is not frozen, so `LogPath` is
  recorded and the live worker's artifacts are referenced from birth. That closes the class rather
  than observing it. Phase 2.3's safety argument depends on this being a real fix, and with the
  retention-cutoff floor now specified there, the two protections are independent rather than
  stacked on one assumption.

Host attribution for the compaction path is inference, not evidence (VERIFY R10 — the 2026-07-29
backup sits 46 bytes under the trigger). The mechanism is confirmed; the claim that it fired on this
host is not.

Tests. `internal/jobindex/file_store_test.go` **does not exist** (the package has `catalog_test.go`
only), so this is a new file with no in-package precedent.

**Sanity Check:**

- A test asserts a tier-2 compaction pass records every dropped record in the form Phase 2.1's audit
  reads, and that the audit then reports those files as unreferenced.
- A test asserts a caller can distinguish a tombstoned no-op from an applied patch **without**
  receiving an error, and that `AdoptAndCleanup` still proceeds to cleanup in that case.
- `go test ./internal/jobindex ./internal/runtime/docker` exits 0.

**Rollback:** revert. No on-disk format change if the drop record rides existing fields.

### Phase 2.3: Make the cap honest without making live files evictable [TODO]

`ci-runner`. Lands last, verified by the audit built in 2.1.

`artifacts.go:435` gates eviction on `if !expired && total <= s.policy.TotalCapBytes`, where `total`
accumulates only over non-tombstoned records. Measured: 2239 MiB on disk against a 2048 MiB cap, only
693 MiB visible to it.

**The change is narrow, and the narrowness is the point.** Counting every file in `total` and making
every file a deletion candidate are separable, and only the second creates blast radius:

- **`total` becomes disk-derived** — every file in both directories counts, so the cap stops lying and
  success criterion 1 is met.
- **`candidates` stays catalog-derived** — `referenced` remains the deletion guard.
- Unreferenced files gain a cap-driven eviction class ordered by mtime, **excluding the in-flight
  temporary patterns**.

**Sweep ordering is specified, and it is load-bearing.** Run the unreferenced sweep FIRST, re-derive
`total` from disk, and only then run the catalog candidate loop. Without this, the deficit that
unreferenced bytes create is paid out of referenced, in-window artifacts: the catalog loop at
`artifacts.go:433` runs before `cleanupOrphans` at `:462`, its guard decrements `total` per deletion
and re-tests (`:455-459`), so with `total` disk-derived it deletes roughly the oldest **191 MiB** of
referenced, finalized, still-in-retention artifacts on the measured host numbers — files it should
never have touched — and stops, having never removed the orphan bytes that caused the overage. It
recurs on every pass where unreferenced bytes regrow past the cap. Found independently by both
consensus reviewers (`CONSENSUS-codex.md` F1, `CONSENSUS-fresh.md` F2).

**There is no age-based eviction floor. Retired — it was wrong.** Revision 3 set the floor at the
retention cutoff; `CONSENSUS-r3.md` showed it contradicts success criterion 1 by construction and
mutually defeats the R1 sweep ordering, because age-based protection and cap enforcement over
in-window bytes are the same axis pulling opposite ways.

**The precedent settles the precedence, and it is the opposite of the floor.** `json_log.go`'s
`cleanupLocked` runs two passes: pass 1 removes age-expired files (`path != s.path &&
info.ModTime().Before(cutoff)`), and the survivors — files INSIDE the retention window — are exactly
the population the cap loop then evicts from, oldest-first, until under cap. So in this codebase's
own sibling policy **the cap is authoritative over retention**: retention is a floor for
deletion-by-age, not a shield against the cap. Criterion 1 stands as written.

**Protection is by identity, not by age.** `json_log.go` protects its live file with `path != s.path`
— an identity test — and excludes `.tmp` by construction via its filename filter (`:331`). The
artifact sink needs the same shape: the in-flight temporary patterns are a filename filter, and the
adoption-orphan class needs an identity-based answer. **What that answer is, is the central question
for the Stage 2 design gate** — revision 3's `indexAdopted` proposal was an attempt at it and failed,
both as unimplementable against `Validate`/`Upsert`/`Merge` and on a false premise (`indexAdopted`
never patches `LogPath`).

Why not the disk-enumeration shape revision 1 proposed: three classes of file are unreferenced **by
construction**, not by drift, and `cleanupOrphans`' age floor is all that protects them. In-flight
bytes live in `os.CreateTemp(".ci-runner-log-*.tmp")` (`artifacts.go:111`) and siblings at `:146,228`,
none ever a catalog path; `OpenLog` records the final path at `:102-104` but only moves onto it in
`atomicLogFile.Close()` (`:566`), so during a live job the referenced path may not exist while live
bytes sit unreferenced. On Windows deletion returns a sharing violation that poisons `cleanupErrors`;
on POSIX it is silent loss of an in-progress log.

The `json_log.go` precedent is safe **because** it filters to `controller-*.jsonl` (`:331`), excluding
`.tmp` by construction, and skips one known-live path by identity (`:361-363`). The artifact sink has
N concurrently-live workers and no single active path; the only thing that can play that role is
`referenced`. The precedent argues *for* keeping it. Two further hazards the narrow shape avoids:
`indexAdopted` → `Merge` cannot re-open a tombstoned pool+runner key, so an adopted live worker's
artifacts can be unreferenced from birth (`artifacts.go:345-357`, `catalog.go:82-84`) — which is why
2.2 precedes this phase; and a transient stat error `continue`s before `referenced` is populated
(`artifacts.go:411-414`), making a live referenced file unreferenced for that pass.

Do not port the precedent's wart: its eviction loop `continue`s past the active file without
subtracting its size from `total` (`json_log.go:361-367`), so one active file larger than `TotalCap`
deletes everything else and still exits over cap.

Correction carried: `EXPLORE-artifact-retention.md:130` states the `saturatingAdd` effect backwards. A
saturated `total` makes the `:435` test permanently **false** — eviction always proceeds — not true.

**Sanity Check:**

- **Negative test, the one that matters:** a file referenced by an open record, and an in-flight
  `.ci-runner-log-*.tmp`, both **survive** a cap-driven sweep that is well over cap.
- A test asserts a transient stat error does not make a referenced file evictable.
- A positive test asserts unreferenced, non-temporary files over cap are evicted oldest-first until
  under cap.
- `go test ./internal/runtime/docker` exits 0; `golangci-lint run` exits 0; `go-fuzz` and
  `go-windows-build` CI jobs pass.
- Run Phase 2.1's audit on the live host before and after; the before-run is the evidence baseline.

**Rollback:** revert. **Irreversible if it deletes wrongly** — hence the audit-first ordering, the
negative tests, and the narrow shape.

---

## Operator prerequisites

The auto-mode classifier refuses agent writes to the chezmoi source, so these are operator actions.
**Only Phase 1.3 waits on them.**

1. Commit the two uncommitted source files; fast-forward the source `main` to `origin/main` (12
   behind, 0 ahead); leave the source on `main`.
2. **First `chezmoi apply`.** Real blast radius, corrected: beyond rewriting three `autoMode.allow`
   rules and adding twelve (VERIFY R2), the same apply creates from nothing **81 `permissions.allow`
   rows, 166 force-deny + 272 component-deny rows, and 8 ask rows** — the deployed file currently has
   only `defaultMode` and `additionalDirectories`.
   **This apply will not unblock read-only `gh`.** The 81-row floor is 100% shell rules and
   `classifyAllShell: true` suspends every one of them in auto mode. It is a prerequisite for layer 1
   of Phase 1.3, not a fix.
3. **Second `chezmoi apply`,** after Phase 1.3's source edits merge. Phase 1.3's own Sanity Check
   requires it.
4. Optional, low yield: further `claude auto-mode critique` runs. Two of ~15 have generated, each
   truncating at a different length and ranking a different finding first; both top findings are
   already folded into Phase 1.3 items 5 and 6.

## Blast radius

**MEDIUM-HIGH.** Three repos. Phase 2.3 changes deletion semantics against live operator data — now
narrowed, audit-verified, and negative-tested. Phase 1.3 changes a fleet-wide permission surface and
has a stated may-not-work branch. Phases 1.1, 1.2, 2.1, 2.2 are contained.

## Decisions

| Decision | What it changes | Basis |
| :-- | :-- | :-- |
| Narrow cap fix: disk-derived `total`, catalog-derived candidates | Phase 2.3 shape | `PLAN-REVIEW.md` 1/2/16, verified against `artifacts.go:111,146,228,411-419,566`; operator-approved |
| Phase-gate the Docker probes | Phase 1.1 scope | `PLAN-REVIEW.md` 6; precedent at `doctor_inspector.go:147`; operator-approved |
| Stage 2 order: audit → catalog → cap | Sequencing | `PLAN-REVIEW.md` 13/3; operator-approved |
| Audit drops the Docker precondition; purge retains it | Phase 2.1 `[FALLBACK]` | Hazard analysis; override invited |
| `autoMode` only, no CLAUDE.md leg | Phase 1.3 scope | Official docs C2/C3, fetched this session |
| Narrowing in `soft_deny`, not `allow` prose | Phase 1.3 structure | `RESEARCH-automode-authoring.md` B3, four confirmations |
| No install-root prune | Out of scope | `docs/releases.md:137` floor reading |
| Two stages | Sequencing | Process judgment — no external authority |

## Open questions

- `claude auto-mode critique`'s remaining findings: unrecovered. Both recovered top findings are
  folded in. Low yield, not a blocker.
- Which rule scored the original `gh issue list` block: UNVERIFIED and unverifiable with shipped
  tooling. Phase 1.3 item 6 is a mechanism, not a proof.
- Precedence among multiple `autoMode.allow` entries: NOT DOCUMENTED.
- Whether `permissions.deny` is genuinely unaffected by `classifyAllShell`: DERIVED, not stated
  (`RESEARCH-automode-authoring.md:263-270`). Layer 1 of Phase 1.3 rests on it.
- How often a pool+runner key is reused inside the 14-day tombstone window: mechanism confirmed,
  frequency untraced.

## Review-closure table

`PLAN-REVIEW.md` findings and where revision 2 addresses them.

| Finding | Sev | Closed by |
| :-- | :-- | :-- |
| 1, 2, 2b, 14, 15, 16 | BLOCKING/MAJOR | Phase 2.3 rewritten to the narrow shape; negative Sanity Checks added; 2.2 precedes it |
| 3, 4 | BLOCKING | Phase 2.2: no tombstone conversion, record the drop for the audit; 2.1 precedes it |
| 5 | BLOCKING | Phase 1.3 Sanity Check uses `tools/claude-settings-compose/test.ps1` |
| 6, 7 | BLOCKING/MAJOR | Phase 1.1 adds phase-gating; `doctor.go:131` and `:119` both named |
| 8 | MAJOR | `grep -c` replaced by a property-asserting test |
| 9 | MAJOR | Phase 1.3 states the may-not-work branch and escalates to a `PreToolUse` hook |
| 10, 11 | MAJOR | Prerequisite 2 states the real blast radius and that it does not fix the block |
| 12 | MAJOR | Rollback line on every phase; `claude auto-mode reset` cited |
| 13 | MAJOR | Stage 2 reordered audit-first |
| 17 | MAJOR | Phase 1.3 item 4 reconciles against source `autoMode.allow[13]` |
| 18, 23c | MAJOR | Phase 2.1 specifies both modes; five-flag check |
| 19 | MAJOR | Phase 2.2 signals the no-op out-of-band, not as an error |
| 20 | MAJOR | Lint and CI-gate Sanity Checks on every code phase |
| 21 | MAJOR | Phase 1.2 asserts the exact superseded string is gone |
| 22 | MAJOR | Second apply added as prerequisite 3 |
| 23 | MAJOR | Phase 2.1 branches `provisioning` from `origin/main` |
| 23b | MAJOR | "Thread" retired; design gate covers all three Stage 2 phases |
| 24 | MINOR | `logs.go:29` corrected |
| 25, 26 | MINOR | Both phases state the test files and fakes do not exist |
| 27 | MINOR | Phase 2.3 names the precedent's wart and declines to port it |
| 28 | MINOR | Grep retained; spoofability noted, property test is the real gate |
