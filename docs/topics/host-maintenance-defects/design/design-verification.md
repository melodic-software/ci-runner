# VERIFY — adversarial review of the Stage 2 design gate

Two independent fresh-context reviewers, each briefed to REFUTE rather than confirm and to default to
"refuted" when uncertain, verified `design.md`'s D1-D5 against source. Neither was given the
document's rationale as evidence.

**This file overrides `design.md` wherever they conflict.** Precedence for Stage 2 is now:
`design-verification.md` > `design.md` > `PLAN-REVIEW.md` > `VERIFY.md` > the EXPLORE/RESEARCH sidecars.

Baseline: both reviewers independently confirmed `internal/runtime/docker` and `internal/jobindex`
are untouched by `055c196` (last commit touching them is `8df1100`), so their findings hold against
the current branch.

## The headline, and it is not a detail

**Every one of D1-D5 survives as a mechanical claim. Three fail on composition.** The design was
verified against the code as it exists and never against the code as Phase 2.3 would leave it — the
same defect class that let Phase 1.1 ship without reaching its own success criterion. Recorded here
because it is now the second instance in this workstream.

## Findings that change a decision

### F1 — "over-population of `referenced` is inert" is FALSE under 2.3's own cap design

`design.md` D1 justifies itself as fail-safe: "over-population of `referenced` only ever protects
files from deletion." True against current code — nothing reads `referenced` except
`artifacts.go:664`, verified by repo-wide grep.

It does not survive `design.md`'s own sweep ordering. Step 2 derives `total` **from disk**; step 3
evicts **only unreferenced** files. So every extra `referenced` entry counts toward the cap while
being exempt from paying it. Protecting X forces extra eviction of Y, or makes the cap unreachable.
**Over-population is redistribution, not inertness.**

Same defect reached independently from the other side: temps are on disk, count toward a disk-derived
`total`, and D2 forbids evicting them. N live workers × `MaxFileSizeBytes` of log temps is a
permanent deficit the cap lane cannot pay, so step 5 pays it out of referenced, in-window, finalized
artifacts.

**Consequence for D1, D2, D3: all three still land, but none of them may be justified as fail-safe.**
The cap design must account for a protected-but-counted class explicitly.

### F2 — the sweep ordering is necessary, not sufficient

The arithmetic was verified sound: `:434-436` uses `continue` not `break`, the `:455-459` decrement is
saturation-safe and fires only after a successful removal and tombstone.

What the ordering does **not** close: any unreferenced or unevictable byte still on disk at step 4 is
paid out of referenced in-window artifacts at step 5, because step 5 decrements `total` only by
`candidate.size` (catalog-derived, `:420`) while comparing against a disk-derived total. Three
sources:

- temps, excluded by D2;
- files whose `os.Remove` failed in step 3 (`cleanupOrphans:679-681` records the error, leaves the file);
- **files of records dropped by the validation `continue` at `:411-414`.** D3 puts their paths into
  `referenced` (protected from step 3) but keeps the `continue` for candidacy, so they are never
  candidates either. Their bytes sit in the disk total, unevictable by **both** lanes, forcing
  deletion of healthy artifacts every pass until the stat error clears. `design.md` D3 keeps the
  `continue` "for candidacy and `total`" without noticing a disk-derived `total` makes those bytes
  permanently unpayable.

### F3 — the class enumeration is incomplete; four more producers

`design.md`'s "Completeness argument" is refuted. Its sub-claim is confirmed — `referenced` has one
write site (`:417`) and one read site (`:664`) — but the three classes do not exhaust "live file,
unreferenced":

- **F3a — degraded-base final artifacts.** `r.metadata` returns bare `ArtifactMetadata{ContainerID:
  id}` when `ContainerInspect` errors **or** `inspect.Container.Config == nil`
  (`runtime.go:867-869`). `WriteDiagnostics` publishes the final file at `:181` and only **then**
  upserts at `:192`, where `Merge` rejects the empty `PoolID` — file on disk, error returned, no
  record. `WriteResourceEvidence` (`:201-268`) contains **no `s.jobs` call at all**: it publishes at
  `:255` and returns `(true, nil)`.
- **F3b — resource-evidence ordering window, on the healthy path with no error anywhere.** `finalize`
  calls `captureResourceEvidence` (`runtime.go:707`) before `captureDiagnostics` (`:715`), so
  `resources.json` is published while `DiagnosticPath` is still unset; `ResourceEvidencePath` returns
  `("", nil)` and the `path != ""` guard at `:416` skips it.
- **F3c — catalog compaction**, which `file_store.go:355-357` documents outright. The `record.Open`
  guard does not save it: `Finalize` sets `Open=false` (`artifacts.go:289-293`) while the container is
  still alive — `ContainerRemove` is not until `runtime.go:746`.
- **F3d — snapshot staleness.** `cleanup` loads at `:387` and sweeps at `:463-464` with N
  lock-taking `Upsert` calls between. `FileStore.Load` releases its lock immediately
  (`file_store.go:90`), and `finalize` runs on its own goroutine (`runtime.go:593`), so a
  `WriteDiagnostics` publish inside that window leaves a live final file unreferenced.

**This retires `design.md`'s claim that `OpenLog` failing closed means "there is no degraded-base
residue."** That generalizes from one writer to three. The `OpenLog` mechanism itself is confirmed —
`FileStore` is the sole production `Store` (`bootstrap.go:67`, `controller_main.go:123`,
`var _ Store = (*FileStore)(nil)` at `file_store.go:420`) — but the other two writers publish before,
or entirely without, the catalog gate. No test covers degraded metadata.

### F4 — D1's derivation matching is confirmed, with one real divergence

Confirmed, and two suspected divergences were attacked and cleared: `ContainerID` (both full 64-hex,
truncated identically at `:747-749`) and `StartedAt` (written `RFC3339Nano` at `runtime.go:324`,
parsed `RFC3339Nano` on both sides — round-trip lossless).

The divergence is the degraded branch, per F3a. Compounding it: **three independent `r.metadata`
calls exist per container** — `runtime.go:754` (captureLogs), `:706` (finalize/resource evidence),
`:792` (captureDiagnostics) — so one container's artifact trio can split across two base names.

### F5 — D3's stated safety reason is wrong

`design.md` argues an out-of-root reference is inert. That conflates "outside *a* root" with "outside
*both* roots". A record whose `LogPath` lies inside `diagnosticDirectory` fails
`artifactSizeWithin(s.logDirectory, …)` at `:407`, yet canonicalizes to **exactly** an entry that
`cleanupOrphans(s.diagnosticDirectory, …)` at `:464` enumerates. Nothing constrains root membership:
`Validate` (`catalog.go:150-154`) and `mergePath` (`:193-201`) check absoluteness only.

Direction is protect-only — no exposure path was constructible, since D3 only adds map keys. The
consequences are a pin (anyone who can write `jobs.json` makes a chosen in-root file permanently
undeletable) and F1's redistribution. The existing test
`TestCleanupRefusesIndexedPathsOutsideConfiguredRoots` (`artifacts_test.go:198-221`) places the
escaping path in a *third* directory, so the cross-root collision is untested.

Low-severity, same function: `canonicalPath`'s unconditional `ToLower` (`:728`) collapses case while
`artifactBaseName` preserves it, so on a case-sensitive filesystem — which the repo builds for
(`internal/state/fs/replace_other.go:10`) — referencing `A.log` protects a distinct `a.log`.

### F6 — D4 misattributes the capability, and the atomicity problem is unresolved

Mechanically confirmed: `encodeWithinCapacity` is the only caller of `compactOldestCompleted`
(`file_store.go:317`), its caller `saveUnlocked` (`:240`) does hold `s.directory` and already does
`MkdirAll`/`Harden`/`Verify`/`CreateTemp` there, and there is no lock re-entry **provided the journal
writer uses raw `os` calls** — `saveUnlocked` runs with the lock held and `Locker` has no reentrancy,
so any call back into an exported `FileStore` method would deadlock.

Refuted or unaddressed:

- `encodeWithinCapacity` is **pure over `*Catalog`** — no directory, no filesystem access. The
  capability belongs to `saveUnlocked`. Dropped identities therefore need **two** signature changes,
  accumulated across `encodeWithinCapacity`'s retry loop (`:304-321`), which can call
  `compactOldestCompleted` repeatedly.
- **Atomicity is genuinely broken and `design.md` does not say so.** The journal is a separate file
  from `jobs.json` with no way to pair them: written before `ReplaceFileAtomic` (`:275`), a failed
  replace leaves phantom entries; written after, a crash in the window loses exactly the record the
  journal exists to keep.
- **Failure policy unstated.** A journal write error either fails the whole save — recreating the
  permanent-write-failure livelock compaction exists to prevent — or is silently swallowed.
- **The byte cap is not cheap.** Oldest-first truncation means read-parse-drop-rewrite of the whole
  journal on every save, inside the lock, on the hot path. And `maximumJobState` is `8<<20`, so
  reusing it doubles the state directory's worst case, with no analogue of `jobs.json`'s asymmetric
  load tolerance (`maximumJobStateLoad = 4 * maximumJobState`).

### F7 — D5's root cause was misidentified, and its blast radius overstated

**The actual root cause: `ActiveJob` is the only `Record` reader that does not filter tombstoned
records.** `FindByJobID` (`file_store.go:102`) and `FindByRunner` (`:118`) both carry
`&& record.TombstonedAt == nil`; `ActiveJob` (`:136-139`) does not, so it reaches `:140` and
evaluates `active` on a frozen record. That asymmetry — named nowhere in `design.md` — is the root of
both halves of D5.

`design.md`'s stated reason was wrong: `active` at `:140` reads `JobID`/`JobStartedAt`/`CompletedAt`
only and never inspects `TombstonedAt`, so "a tombstoned record is terminal, so `active` is false"
does not follow. The opposite is what trips `ErrConflict`.

**The consequence is a lost `JobID` field, not a lost busy classification.** `enrichWorkerJobs`
(`reconciler.go:1372-1385`) only ever *adds* `WorkerBusy` and never clears it. Worker state comes
independently from the Docker runtime's in-container state probe (`runtime.go:523-526`), and a failed
probe degrades to `WorkerStarting`, never idle. Scale-down protection keys on that `State`.

**Reachability, precisely.** The record state — `FinalizedAt` set, `Open` false, `JobID` and
`JobStartedAt` set, `CompletedAt` permanently zero — is **unconditionally reachable**: the two
timestamps have independent producers with no ordering relation (`runtime.go:735` versus
`scaleset/official.go:259`), `Finalize` never inspects `CompletedAt`, and `official.go:229-249`
intentionally drops completions it cannot attribute to an exact runner (`official.go:27-29`). The
candidate filter at `artifacts.go:423` genuinely lacks `file_store.go:377-379`'s guard, and `Merge`
preserves `JobID` through tombstoning.

But the **hard error** additionally needs `ActiveJob` called for that key *after* the tombstone, and
its sole caller iterates the live Docker inventory. Runner names are effectively unique
(`reconciler.go:1086-1097`), so no new worker queries a dead worker's key. The reachable route needs
a live container **missing from the inventory** handed to `AdoptAndCleanup` — the O2 gap — which lets
`reconcileStaleOpen` (`artifacts.go:376-379`) force-close it, plus cap pressure to tombstone it
before it reappears.

So: live-fleet defect, yes — but gated on inventory incompleteness plus cap pressure, the same
precondition as class 2, not reachable on the lost-completion fact alone.

### F8 — D2 survives mechanically; its "costs no leak" conclusion does not

Confirmed exhaustively: all three temp sites are in swept directories, there is no fourth
`os.CreateTemp` in either root (the other two are in the state directory), `ReplaceFileAtomic` creates
no intermediate on either platform, and a prefix+suffix filter is total because `os.CreateTemp`
substitutes the single `*`.

Two defects `design.md` does not mention:

- **Neither lane runs at all when the catalog is absent.** `cleanup` returns `nil` on `ErrNotFound`
  at `:387-390`, **before** `cleanupOrphans` at `:463-464`. A missing `jobs.json` means zero orphan
  sweep, zero temp collection — and a Phase-2.3 cap lane placed inside `cleanup` is equally dead. Same
  for an over-limit load (`file_store.go:208-210`) returning at `:391-393`. D2's whole rationale rests
  on "the age lane still collects them"; the age lane is conditional on a catalog load D2 never
  checks.
- Temps are unevictable but still counted — F1.

D2's stated residual checks out: `cleanupOrphans:676` skips only on `ModTime().After(cutoff)`, and
`config.go` validates only `Retention > 0`.

## Pre-existing defects found in passing

Neither is introduced by Stage 2; both are in the loop it modifies.

- **Failed tombstone `Upsert` after successful file removal skips the `total` decrement**
  (`artifacts.go:451-454` continues after `:438-442` already removed the files), over-deleting
  subsequent candidates in the same pass.
- `saturatingAdd` saturation would turn the candidate loop into delete-everything, but needs ~2^64
  accounted bytes. Unreachable; noted only.

## What this changes for implementation

1. **Phase 2.3 must define what a protected-but-counted byte does to the cap** before D1, D2, or D3
   is implemented. F1 and F2 are the same defect seen from three directions, and no ordering fixes
   it — it is an accounting question, not a sequencing one.
2. **`ActiveJob`'s missing tombstone filter is the cheapest real fix in Stage 2** and is independent
   of everything else. It is a one-line change matching two sibling readers. Do it first.
3. **`cleanup`'s `ErrNotFound` early return gates the entire orphan lane** and would gate the new cap
   lane. Fix before adding anything inside `cleanup`.
4. **D4 needs redesign, not just a bound.** The atomicity gap and the failure policy are unresolved,
   and the two-signature-change plus retry-loop accumulation is real work `design.md` does not
   describe.
5. **F3a/F3b are live-file-loss classes that no proposed decision covers.** They are not addressed by
   D1, which derives from adoption metadata that the degraded branch does not produce.
