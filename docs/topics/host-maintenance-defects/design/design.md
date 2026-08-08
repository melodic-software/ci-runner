# DESIGN — Stage 2 gate (phases 2.1, 2.2, 2.3)

Design gate declared by `PLAN.md` §Staging. Read-only pass; no source edits made.

Verified against `fix/per-probe-deadlines` @ `dfc22a3`. `git diff cfd92ac..HEAD --stat` touches
`internal/app` and `internal/host` only, so `CONSENSUS-precedence.md`'s verification baseline
(`cfd92ac`) still holds for `internal/runtime/docker` and `internal/jobindex`.

Precedence unchanged: `PLAN-REVIEW.md` > `VERIFY.md` > EXPLORE/RESEARCH sidecars. This file adds a
layer above all of them for the three Stage 2 phases only.

## STATUS: superseded in part by adversarial review

Two independent fresh-context reviewers verified D1-D5 against source. **Every decision below
survived mechanically; three of them do not survive composition with Phase 2.3's own cap design.**
The verified findings are in `design-verification.md`, which overrides this file wherever they conflict.

Read that file first. In particular, do not implement D1 or D3 on the strength of the "fails safe /
inert" arguments below — both are refuted under a disk-derived `total`.

## Headline

**The central question was malformed.** `PLAN.md` §2.3 and the handoff both ask for *one* identity
signal — "the artifact sink's equivalent of `json_log.go`'s `s.path`". Every candidate in
`CONSENSUS-precedence.md`'s inventory fails against that framing because "live-but-unreferenced" is
not one class. It is three, with three producers and three answers.

**And the sink already owns a stronger identity signal than `json_log.go` does.** `artifactBaseName`
(`artifacts.go:732-751`) is a pure function of `ArtifactMetadata`. `ArtifactMetadata` is built by
`metadataFromLabels` (`runtime.go:873-881`) from container labels stamped at creation
(`runtime.go:324`). The same function serves both `OpenLog`'s caller (`runtime.go:754-755`) and the
adoption inventory (`runtime.go:269`), so a live container's final artifact paths are **derivable
from the Docker inventory alone, with no catalog read**, and the derivation is stable across
controller restarts. `json_log.go`'s `s.path` is in-memory and dies with the process; this does not.

So the answer to the gate's central question is a **two-part** identity:

1. **Path-class identity** — the `.ci-runner-*-*.tmp` filename patterns, present from `os.CreateTemp`
   onward with no window, for in-flight bytes.
2. **Live-worker identity derived from the adoption inventory** — `artifactBaseName(metadata)` for
   every container in the inventory `cleanup` is already handed, unioned into `referenced`.

Part 2 is the load-bearing one. It is catalog-independent, which is exactly why it survives the
tombstone collision (class 2) and the validation drop (class 3) that defeat every catalog-derived
signal.

---

## The three classes, enumerated against source

### Class 1 — in-flight temporaries, unreferenced by construction

`.ci-runner-log-*.tmp` (`artifacts.go:111`), `.ci-runner-diag-*.tmp` (`:146`),
`.ci-runner-resources-*.tmp` (`:228`). Never in `referenced` (`:415-419` reads catalog paths only).
`cleanupOrphans` (`:656-684`) enumerates every entry with no name filter, unlike `json_log.go:331`.

**Today the hazard is latent, not live.** The only deletion term in the orphan lane is
`info.ModTime().After(cutoff)` (`:676`), and a live temp's mtime is ~now. It becomes live the moment
Phase 2.3 adds a cap-driven eviction class to that lane, because the cap term has no age floor.

### Class 2 — adoption onto a tombstoned pool+runner key

`indexAdopted` (`:345-358`) upserts `PoolID`+`RunnerName`. `Merge` returns the record unchanged on
`existing.TombstonedAt != nil` (`catalog.go:82-84`), so `Open` never sets and no later `OpenLog`
patch can land `LogPath` either. `cleanup` skips tombstoned records before populating `referenced`
(`artifacts.go:404-406`). A live worker's artifacts are therefore unreferenced from birth.

`Validate` enforces one record per `PoolID`+`RunnerName` (`catalog.go:133-137`), and `mergePath`
delegates to `mergeImmutable` (`:200`), which conflicts on a changed path. **That pair is why
revision 3's "record the adopted worker under a key that is not frozen" was unimplementable** — there
is no second key available and the path field is write-once.

### Class 3 — one malformed path drops all three

`artifacts.go:411-414` `continue`s **before** `referenced` is populated at `:415-419`. `logErr`,
`diagnosticErr`, `resourcePathErr`, and `resourceErr` are joined, so any one failure drops all three
of that record's paths. `ResourceEvidencePath` errors on a `DiagnosticPath` that is non-absolute or
lacks the `-diag.tar.gz` suffix (`catalog.go:167-169`), so one malformed diagnostic path exposes a
valid `LogPath`.

**Completeness argument.** `referenced` has exactly one producer, the loop at `:403-419`. A file in
a swept directory is unreferenced iff (a) no record names it — class 1 and class 2 — or (b) a record
names it but the loop skipped population — class 3 (validation `continue`) or the tombstone skip at
`:404`, which is class 2's general form and is intentional for genuinely dead records (`:438-442`
already removed their files). No fourth producer exists, so the enumeration is complete.

---

## The discriminating test

For any candidate signal: **is there a window in which a file holding live bytes is visible to the
sweeper without a liveness signal the sweeper can read?** Not "is the signal clever" — is the window
closed. `CONSENSUS-precedence.md` Follow-up 2 names this as the property that makes `json_log.go`'s
identity-skip sufficient (single-mutex create-and-register, `json_log.go:292` → `:310-311`).

| Signal | Window | Verdict |
| :-- | :-- | :-- |
| Temp filename pattern | none — `os.CreateTemp` creates the name atomically | **closes class 1** |
| `artifactBaseName` from adoption inventory | none — label stamped at container creation, `runtime.go:324` | **closes class 2** |
| Per-path `referenced` population | none — pure reordering of existing work | **closes class 3** |
| `Record.Open` | set at `:101-104`, before `CreateTemp` at `:111`; names the final path, never the temp | fails class 1 |
| mtime | the whole cap lane has no age term | fails class 1 under 2.3 |
| Un-tombstoning via `Merge` | n/a | blocked by `Validate`/`mergeImmutable` |

---

## Design decisions

### D1 — `referenced` gains a second, catalog-independent producer

`cleanup` currently receives `adopted map[string]struct{}` (`artifacts.go:386`), built by
`indexAdopted` from the `[]ArtifactMetadata` its caller already holds (`:300-301`, `:327-328`). Pass
the metadata slice through instead of (or alongside) the ID set, and for each entry add
`artifactBaseName(metadata)`-derived paths — `<base>.log` in `logDirectory`, `<base>-diag.tar.gz` and
`<base>-resources.json` in `diagnosticDirectory` — to `referenced` **before** the record loop runs.

Properties:

- Closes class 2 at the sweep rather than at the catalog, so no `Merge`, `Validate`, or schema change
  is needed and `PLAN.md`'s "do not convert the drop into a tombstone" / "do not signal `Merge` as an
  error" constraints are untouched.
- Closes class 3 for live workers specifically — the population no longer depends on the record loop
  reaching `:415`.
- Derivation-by-convention has an in-repo precedent that documents the reasoning:
  `ResourceEvidencePath` (`catalog.go:159-172`) derives the sidecar path rather than extending
  schemaVersion 1.
- Fails safe. Over-population of `referenced` only ever protects files from deletion.

**Load-bearing check, run and cleared.** `r.metadata` degrades to `ArtifactMetadata{ContainerID: id}`
on inspect error (`runtime.go:864-871`), and `runtime.go:754-755` does pass that straight into
`OpenLog` — so a degraded base name (`worker`, epoch timestamp) reaching disk looked possible, and it
would not match the healthy adoption derivation. It cannot happen: degraded metadata has an empty
`PoolID`, `OpenLog`'s first act is `s.jobs.Upsert` (`artifacts.go:102-107`), `FileStore.Upsert`
delegates to `Merge` (`file_store.go:172`), and `Merge` rejects an empty pool or runner name outright
(`catalog.go:70-72`). `OpenLog` therefore returns before `os.CreateTemp` at `:111` and no file is
created. The same holds for partial label loss, since `metadataFromLabels` reads `PoolID` from a label
too. **`OpenLog` fails closed; there is no degraded-base residue.**

### D2 — the temp filename filter binds the cap lane, and only the cap lane

`cleanupOrphans` keeps its mtime term unchanged for the age lane. The new cap-driven eviction class
Phase 2.3 adds skips any entry matching the three temp patterns.

Rationale: the age lane's mtime floor is `retention`, which doubles as the collector for temps
abandoned by a crashed worker — so excluding temps from the cap lane costs no leak. Filtering both
lanes would leak abandoned temps unless a new floor parameter is introduced, and that parameter has
no defensible value (see O1).

**Stated residual:** a job whose artifact write outlives `policy.Retention` has its live temp deleted
by the age lane. `config.go:774` validates only `Retention > 0`, so nothing forbids a retention
shorter than a job. This is pre-existing behaviour, not introduced by Stage 2. See O1.

### D3 — `referenced` is populated per-path, not all-or-nothing

Move the population at `artifacts.go:415-419` above the joined-error `continue` at `:411-414`, adding
each path whose own validation succeeded. Keep the `continue` for candidacy and `total` — a record
with an unreadable path must not become a deletion candidate and must not contribute a bogus size.

Referencing an out-of-root path is inert: `canonicalPath` of a path outside the swept root cannot
match any entry `cleanupOrphans` enumerates.

This is the mechanism `PLAN.md` §2.3's third Sanity Check ("a transient stat error does not make a
referenced file evictable") asserts but never specified.

### D4 — Phase 2.2's drop journal stays as specified, with its bound named

`PLAN.md` §2.2's resolution is unchanged: `compactOldestCompleted` stays pure over `*Catalog` and
returns dropped identities; the caller of `encodeWithinCapacity` writes them to a bounded journal
beside the state file. `encodeWithinCapacity` (`file_store.go:303-322`) is the only caller path and
already sits under the save path with filesystem access.

The bound is a design decision this gate must make and `PLAN.md` left open: **cap the journal by the
same `maximumJobState` discipline the thing it observes uses** — a byte cap with oldest-first
truncation, not an entry count. An entry count admits unbounded growth as record identity strings
grow; a byte cap cannot, and it reuses a constant the package already reasons about.

### D5 — D1 closes the collision's *artifact* half; its *scheduling* half stays open

`PLAN.md` §2.2 assigns the signal's consumption to `indexAdopted`, "re-opening the collision". D1
supersedes that **for artifact retention only**: with adoption-derived paths in `referenced`, a live
worker on a tombstoned key keeps its artifacts without the record being re-opened, and the record
stays frozen as the tombstone intends. `Merge` still returns `nil` and `AdoptAndCleanup` still
proceeds to cleanup, satisfying both §2.2 constraints.

**The collision has a second half D1 does not reach, and it is outside artifact retention.**
`FileStore.ActiveJob` (`file_store.go:125-147`) resolves worker busy-state from the same frozen
record, and `reconciler.go:1375-1382` sets `model.WorkerBusy` from its answer. Two consequences,
both pre-existing rather than introduced here — D1 simply declines to fix them, so they must be
stated rather than assumed closed:

- A tombstoned record is terminal, so `active` (`:140`) is false and the newly adopted live worker
  **reads as not busy**. A scheduling bug, not a data-loss bug.
- A record that was finalized while its job completion never arrived can still be tombstoned — the
  cleanup candidate filter (`artifacts.go:423`) requires only `!Open`, `!isAdopted`, and a non-zero
  `FinalizedAt`, and does not carry `compactOldestCompleted`'s `CompletedAt` guard
  (`file_store.go:377-379`). `ActiveJob` then returns `ErrConflict` (`:141-143`), which
  `reconciler.go:1376-1377` propagates as a hard error, aborting worker enrichment for that pass.

  **Reachability verified, and the two timestamps are genuinely independent.** `FinalizedAt` is
  written by the artifact sink's own `Finalize` (`artifacts.go:290-293`), on container exit.
  `CompletedAt` arrives only from a GitHub job-completed message routed through
  `EventSink.JobCompleted` (`catalog.go:212-214`), driven by `official.go:258-262`. Nothing sequences
  the two. Worse, `official.go:229-249` *deliberately drops* completions it cannot attribute to an
  exact runner — "valid canceled-before-assignment events are not runner-indexable and are
  acknowledged without calling this method" (`official.go:27-29`) — so a `CompletedAt` that never
  arrives is a designed-for case, not a fault. Any such record whose artifacts age past retention
  becomes a tombstone candidate, and the `ErrConflict` fires from then on.

  This is why 2.2 must stay in scope: it is a live-fleet defect reachable through a documented,
  intentional event-drop path, not a rare race.

Neither is an artifact-retention defect, so neither belongs in 2.3. **Recommendation: keep Phase 2.2
in scope and let it own both**, with a re-open or an equivalent that satisfies `Validate` and
`mergeImmutable`. Phase 2.1's audit should additionally report adopted containers whose pool+runner
key is tombstoned — observability, not remediation, and it belongs in 2.1's report rather than in
`Merge`'s signature.

Ordering check, run: an adoption onto a tombstoned key leaves `ContainerID` empty on the frozen
record, so the `adopted[record.ContainerID]` lookup at `artifacts.go:422` cannot match. That is
harmless only because `:404-406` skips tombstoned records before the candidate filter is reached.
That ordering is load-bearing — a change that moves the tombstone skip below the candidate filter
would make an empty-`ContainerID` live worker candidate-eligible.

---

## Sweep ordering, restated with D1 folded in

Phase 2.3's ordering (`PLAN.md` §2.3, from `CONSENSUS-codex.md` F1 / `CONSENSUS-fresh.md` F2) stands
and gains a step:

1. Build `referenced` = adoption-derived paths ∪ per-path catalog-derived paths (D1 + D3).
2. Derive `total` from disk over both directories.
3. Sweep unreferenced, non-temporary files oldest-first until under cap (D2).
4. Re-derive `total` from disk.
5. Run the catalog candidate loop (`artifacts.go:433-460`) unchanged.
6. Run the existing age-based `cleanupOrphans` (`:461-465`) unchanged.

Step 1 preceding step 3 is what makes step 3 safe; steps 3-4 preceding step 5 is what stops the
unreferenced deficit being paid out of referenced in-window artifacts.

---

## Phase ordering: unchanged, and D1 explains why more cheaply

`PLAN.md` orders 2.2 before 2.3 because 2.3's safety argument depended on 2.2 fixing the adoption
collision. Under D1 that dependency **weakens** — 2.3 protects adopted workers by itself. The
audit-first ordering (2.1 before both) is untouched and stays load-bearing.

Consequence worth flagging to the operator: **2.2 is now separable from 2.3 for artifact safety
only.** If Stage 2 has to be cut short, 2.1 → 2.3 ships a correct cap without waiting on 2.2. It does
not make 2.2 optional — per D5, 2.2 still owns the drop journal and the two scheduling consequences
of the tombstoned-key collision, and the second of those (`ActiveJob` returning `ErrConflict` into
`reconciler.go:1376`) aborts worker enrichment, which is a live-fleet defect rather than a
housekeeping one.

---

## Open questions this gate raises

- **O1 — should `config.go:774` validate `Retention` against a floor?** D2's residual is a live
  worker's temp deleted when `retention` < job duration. A validation floor is the root-cause fix and
  is a one-line config change, but the floor's value needs the real self-hosted GitHub Actions job
  duration ceiling, which is an upstream fact this gate did not verify. **Deferred with trigger:**
  raise it if any pool's configured retention is ever set below 24h. **Trigger status: not tripped.**
  The deployed host config sets `retention: 336h` on both the `logs` and `diagnostics` classes —
  fourteen days — against a `cleanupEvery: 24h`. `artifacts_test.go` uses `Retention: time.Hour`, and
  `config.go:774` validates only `> 0`, so a tripping configuration is invited by the code but is not
  the one in force.
- **O2 — is `CleanupNow`'s complete-inventory contract honoured by callers?** Carried unresolved from
  `CONSENSUS-precedence.md` §Could not verify. D1 raises its stakes: an incomplete inventory now
  costs a missing `referenced` entry, not just a missing candidate exclusion. The failure remains
  fail-safe in one direction only — a container absent from the inventory loses its protection.

## Class 2's real frequency, and what it does to phase ordering

**Runner names are unique per spawn.** `nextRunnerName` (`reconciler.go:1086-1098`) appends
`now.UnixNano()` base-36 plus a monotonic sequence to the configured prefix, so a pool+runner key is
never deliberately reused. The tombstone-collision class therefore needs a *name collision*, which
needs a nanosecond-timestamp and sequence collision inside the 14-day tombstone window. That
answers `PLAN.md` §Open questions' "how often a pool+runner key is reused inside the 14-day tombstone
window" — mechanism confirmed, **frequency effectively nil under the current name generator**.

Consequences, and they cut both ways:

- **D1's urgency drops.** Class 2 is a real hazard but a rare one, so the adoption-derived
  `referenced` is defence-in-depth rather than the load-bearing protection revision 3 assumed.
  D1 is still worth implementing — it is cheap, fails safe, and closes class 3 for live workers as a
  side effect — but Phase 2.3 should not be blocked on it.
- **The ordering claim above is weakened, not reversed.** "2.2 is separable from 2.3" now holds more
  strongly: class 2 barely fires.
- **A new coupling appears.** Because the name carries a timestamp, the protection D1 provides is
  only as good as the label. `artifactBaseName` (`artifacts.go:732-751`) uses
  `metadata.StartedAt`, and `metadataFromLabels` (`runtime.go:879`) *ignores the parse error*
  (`metadata.StartedAt, _ = time.Parse(...)`), leaving the zero time, which `artifactBaseName`
  rewrites to `time.Unix(0,0)`. A malformed `started-at` label therefore yields a *stable but wrong*
  base for both the writer and the adoption derivation — so D1 still matches itself, and the file on
  disk is still protected. Verified, not assumed: both sides call the same function on the same
  labels. Left as a note rather than a defect because the derivation is self-consistent.

**Adoption inventory completeness, checked.** `Runtime.List` (`runtime.go:250-276`) filters on the
managed and host labels with `All: true`, so exited containers are included, and passes every result
to `AdoptAndCleanup` before any watcher can finalize. That is the inventory D1 derives from, and it
is the same one `cleanup` already trusts for `adopted`. D1 therefore adds no new completeness
assumption beyond the one `artifacts.go:422` already makes — which is the honest form of open
question O2 below.

## Answered while running this gate

- **Can `OpenLog` write an artifact whose base name the adoption inventory cannot re-derive?** No.
  `OpenLog` fails closed on degraded or partially-labelled metadata — see D1's load-bearing check.

## Answered, and closed

- **"Does the cap-driven eviction class need a floor at all once identity-based protection exists?"**
  (handoff §Open questions) — **No.** D1 and D2 protect by identity and path class, both
  window-free, so an age floor buys nothing the identity does not already give and would reintroduce
  the criterion-1 contradiction `CONSENSUS-r3.md` retired. The no-floor decision stands.
