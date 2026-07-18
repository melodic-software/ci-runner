# runner-performance

## Brief

### TLDR

Private-repo CI on the two-host self-hosted fleet is queue-bound (GitHub withholds jobs behind a
memory-clamped advertised capacity) and, in medley, checkout-bound (blobless partial clone refetches
all commits+trees per ephemeral job). Fix the capacity clamp and WSL2 sizing, switch medley checkout
to GitHub-recommended filters, and consolidate job fan-out — all on supported surface, worker
security posture unchanged.

### Goal

Cut private-repo CI wall-clock to the acceptance targets below by removing the three measured
bottlenecks, in priority order:

1. Runner-acquisition queue wait — advertised capacity clamps to ~3 vs configured 12
   (`internal/controller/plan.go:414-439` memory-affordable slots; gate reads host
   `AvailablePhysical` via `internal/host/monitor_windows.go:117` while workers are bounded by the
   WSL2 VM at the 50%-of-RAM default, 33GB of 68GB). Verify against live memory readings, then align
   gate basis and size the VM explicitly (`.wslconfig`).
2. medley checkout — replace `blob:none` with `depth:1` (throwaway lanes) or `tree:0`
   (history-needed lanes) per GitHub partial-clone guidance; reduce concurrent-clone contention via
   fan-out consolidation.
3. Job fan-out — consolidate 18-25 tiny jobs per run toward the existing medley `full-tree-gates`
   pattern (shared checkout, per-step `if: always()`); remove the selector preamble hop from the
   critical path where the policy contract allows.

Side flags found during discovery, to file as work items (not perf-gating): provisioning host YAML
drift (2 pools vs deployed 3), scale-set statistics poll failures (×632/24-48h, build/review pools),
cgroup terminal-evidence capture failures (×225), OTel historical queryability gap.

### Constraints

- Worker runtime invariants unchanged (`docs/worker-image.md` — no bind mounts, no named volumes,
  no host filesystem in workers). Any change there is a reviewed ADR, not part of this effort.
- Capacity posture: correctness-first, modest. Keep `maximumConcurrentWorkers` 12 per host; no
  raise before post-fix re-measure. Desk remains a usable workstation.
- Supported surface only: official actions inputs, documented config, `.wslconfig`. No self-managed
  git plumbing replacing `actions/checkout` (it has no reference/alternates support — issue #2303).
- Ownership seams: shared policy → `standards`; selector/reusables → `ci-workflows` (selector
  revisions re-allowlisted via runner-policy review); host config → `provisioning`; controller →
  `ci-runner`; consumer lanes in their own repos. Managed materializations change upstream.
- Public repos stay on hosted runners; only private-repo lanes in scope.
- Route-agnostic restructuring: lanes are shared by both routes, so every change (checkout filters,
  job consolidation, selector shape) must execute identically when the selector falls back to
  hosted (`ubuntu-24.04`), and the selector's documented fail-open/fail-closed semantics and
  runner-policy allowlist contract are preserved.

### Acceptance criteria

Baseline = 2026-07-17 GitHub jobs-API sweep (six active private repos; agent reports in session).
Post-fix: re-run the same measurement over a 2-week window including busy periods.

1. p90 job queue time < 60s across private repos (baseline: github-iac 131s p50 / 909s p90 /
   1363s max; bimodal — idle-fleet runs already 1-2s).
2. medley per-job checkout p90 < 60s (baseline 150-600s; 88% of measured step time).
3. medley `CI Status` full-run wall-clock p90 < 10 min.
4. Fleet job success rate ≥ baseline (~98.9%); zero worker-invariant violations.

Example baseline runs: github-iac 29610002060 (18 jobs queued 594-1363s), medley 29582755099
(7 parallel gates ~590-618s, ~90% checkout), github-iac 29567666117 (idle-fleet contrast, 1-2s).

### Captured assumptions

- The bimodal queue tail is driven by the memory-clamped advertised capacity (mechanism verified in
  code and actions/scaleset README; live clamp-vs-memory correlation not yet captured — first
  implementation step verifies before changing anything).
- Filter switch (blobless → shallow/treeless) materially cuts medley checkout; exact gain unknown
  until measured per lane.
- Cross-repo concurrency contributes to worst-case tails (plausible, not separately confirmed).

### Out-of-scope

- Adding hosts or raising per-host worker count (revisit after re-measure).
- Persistent mounts/volumes/caches in workers; customer-owned cache backend (GHES-only).
- Public-repo workflow changes.

### Deferred questions

- Mirror-mount ADR — trigger: post-fix medley checkout p90 still > 60s. [USER-RESERVED — changes a
  documented security constraint]
- Baking runtimes (.NET/Node/Python) into the worker image — trigger: post-fix data shows setup
  steps dominant; contradicts worker-image.md "deliberately absent" contract. [USER-RESERVED]
- Raising capacity beyond 12 workers/host or aggressive WSL2 allocation. [USER-RESERVED —
  workstation tradeoff]
- Selector-hop removal shape (reusable-workflow output vs per-lane inline) within the runner-policy
  allowlist contract. [/architect]
- Side-flag remediation order (config drift, poll failures, cgroup evidence). [/architect]

## Plan

Evidence base: 2026-07-17 discovery sweep (Brief) + 2026-07-17 architect exploration (4 read-only
agents over ci-runner, medley, ci-workflows, standards, provisioning + live host). Key
plan-shaping findings:

- Gate basis confirmed in code: `availableMemoryHeadroom` consumes host `GlobalMemoryStatusEx`
  (`internal/controller/plan.go:220,496,638-647`; `internal/host/monitor_windows.go:116-117`);
  nothing reads WSL2/Docker memory. Workers run in Docker Desktop's WSL2 backend; `.wslconfig`
  absent on melo-desk-001 (50% default, unmanaged; provisioning has no WSL2 sizing surface today).
- Runbook states desk RAM = 64GB (`provisioning/runbooks/melo-desk-001.md:3`) vs 68GB in Brief —
  Phase 1 reconciles empirically.
- medley checkout — **Brief mechanism claim corrected during planning (2026-07-17):**
  actions/checkout `filter` and `fetch-depth` are independent (`src/git-source-provider.ts` fetch
  options; `filter` overrides only `sparse-checkout` per `action.yml`). With no `fetch-depth`,
  the default depth 1 applies, so lanes with `filter: blob:none` are ALREADY shallow+blobless —
  the Brief's "blobless refetches all commits+trees per job" premise is false. The measured
  150-600s therefore comes from elsewhere — leading hypothesis: on-demand lazy blob fetches
  during working-tree materialization (batched round-trips against the promisor remote, worst for
  full-materialization lanes, amplified under concurrent clones). Phase 4 diagnoses before
  changing anything. History facts stand: only `secret-scan` needs full history+blobs
  (`secret-scan.yml:34-37`) and `detect-changes` needs merge-base commits
  (`ci-status.yml:99-148`); no other lane uses history.
- Selector job runs PARALLEL to `detect-changes` in ci-status (`ci-status.yml:150-177`) — not on
  the dominant critical path. Removal shapes lose `prefer-self-hosted` liveness fallback and/or
  require runner-policy contract entries + org-wide lockstep repin.
- Ownership: medley `.github/workflows/*` locally owned (sync-manifest excludes workflows);
  `.github/standards/runner-policy/*` managed (source = standards); checkouts inside ci-workflows
  reusables (claude-review.yml:196,237) are upstream + SHA-pinned-contract changes.

### Decisions locked pre-plan (user-approved 2026-07-17)

1. Gate re-base mechanism = **static configured worker-memory budget + one-time startup
   cross-check** (probe actual WSL2 VM memory; warn/clamp on drift). BuildPlan stays pure.
2. melo-desk-001 WSL2 VM sized to **40GB** (worst-case worker reservations ≈32GiB + daemon
   overhead; leaves 24GB workstation).
3. Selector-hop removal **deferred with trigger**: revisit only if post-fix measurement shows
   material serial selector wait in standalone workflows (pr-title, actions-lint, osv-scanner,
   secret-scan-scheduled).

### Phase 1: Verify clamp live + materialize baseline [DONE]

Repo: ci-runner (memory tier only — no committed changes). Surface: main session (live host).

1. Reconcile desk RAM (64 vs 68GB) via live read; capture lap-001 RAM (runbook or remote) for its
   later sizing.
2. Sampling script (memory tier, `.work/runner-performance/tools/`): poll every 15s during a busy
   window (induce via `workflow_dispatch` re-run of a high-fan-out github-iac workflow) the tuple:
   deployed `state/observed.json` per-pool `MaxCapacity` + `ResourceSnapshot` memory, active
   worker count, AND the vmmem/WSL2 VM host footprint (process working set). Offline, reproduce
   `affordableWorkerCount` math (`plan.go:638-672`); confirm advertised clamps below pool max
   while the basis is host-physical memory. The vmmem column measures the worker→host-available
   coupling coefficient — it predicts whether ANY host-physical backstop would re-clamp post-fix
   (vmmem is a host process: worker load drags host `AvailablePhysical` down with it). This
   feeds the Phase 2 backstop design decision.
3. Materialize baseline: pull GitHub jobs-API data for baseline runs (github-iac 29610002060,
   medley 29582755099, github-iac 29567666117) into
   `.work/runner-performance/baselines/` as raw JSON + distilled stats.

**Sanity Check:**

- `.work/runner-performance/verification/clamp-correlation.md` exists with a table of ≥10 samples
  (timestamp, availableBytes, computed affordable slots, advertised MaxCapacity per pool) and a
  stated verdict: during the clamp window, the memory term is the binding ceiling (computed
  memory-affordable slots < pool max AND == or bounds the observed advertised value). Universal
  per-sample equality is NOT required — advertised is min(host-count clamp, memory term,
  hysteresis band); other terms may bind outside the window.
- `ls .work/runner-performance/baselines/*.json` returns ≥3 files.
- Live 3-pool worst-case reservation sum recorded (needs the live-only review pool's worker
  memory) — feeds Phase 2 test fixtures and Phase 3 budget value.

**Results (2026-07-18, verified by fresh-context agent, 5/5 criteria PASS):** clamp
REPRODUCED — 67 samples over a 25-min organic busy window; advertised < pool max 12 in
62/67 while demand ≥ 11; exact formula match at window start (computed slots 9 = advertised
9 < assigned 10). vmmem↔host-available coupling measured: slope −0.79 GB available per
+1 GB vmmem, r −0.90 (confirms C1 — no naive host-physical backstop). Desk RAM reconciled:
64 GiB installed = 67.94 decimal GB controller-reported (Brief 68GB / runbook 64GB = same
hardware, unit confusion); lap-001 = 64 GB. WSL2 VM unmanaged (no `.wslconfig`), in-VM
MemTotal 31.0 GiB < 32 GiB worst-case 3-pool reservation sum (8×2 + 2×4 + 2×4 GiB under
host cap 12). Per-busy-worker vmmem growth ≈ 1.6–3 GB (page cache; exceeds 2GiB container
limit at VM level). Evidence: `.work/runner-performance/verification/clamp-correlation.md`
plus `baselines/` (memory tier). Phase 2 gate: PROCEED.

### Phase 2: ci-runner gate re-base + clamp observability [DONE]

Repo: ci-runner. Surface: main session (TDD, judgment-heavy). Depends on Phase 1 confirmation.
Review: code-design, concurrency.

| File | Action | What changes |
|------|--------|-------------|
| `internal/config/config.go` | Modify | New `resources.workerMemoryBudget` (ByteSize) + validation (required-positive when set; schema-version rules per existing pattern at config.go:599-609) |
| `internal/controller/plan.go` | Modify | Memory-slot basis becomes the static budget term: `budget − Σ(ALL active workers: busy+starting+idle — explicit up-front subtraction; today's host reading implicitly excluded busy workers, a static seed must not)` with `allocate()`'s per-start decrement audited for double-counting. Host backstop REDESIGNED, not min()'d: a naive `min(budget, host-headroom)` re-introduces the clamp (vmmem is host-counted — both terms fall together as workers start; stress-test finding C1). Backstop becomes a coarse hard-floor gate on NEW starts using a vmmem-discounted signal (e.g. `AvailablePhysical + VM footprint`, or a low absolute floor worker growth cannot trip) — final shape driven by Phase 1 coupling data. Hysteresis behavior under the discrete budget term specified in tests |
| `internal/host/monitor_windows.go` + `internal/host/monitor_other.go` (or new probe file) | Modify/Create | Cross-check probe: read WSL2 VM MemTotal (`wsl.exe cat /proc/meminfo` or Docker Engine API); warn + clamp effective budget if config > probe; probe failure = warn, use config. Sequenced AFTER Docker Desktop/WSL confirmed up (controller tracks DesktopStatus/RunningWSLCount; VM may not exist at process start, and gaming-mode teardown recycles it) and re-run on VM recycle, not one-time. Non-Windows stub updated so `go test ./...` passes on Linux |
| `internal/telemetry/telemetry.go` | Modify | New gauges: memory headroom + affordable worker count (names per existing `ci_runner.*` conventions); emit a problem/log line when memory clamps advertised below pool max (today silent — plan.go:262-280) |
| `docs/observability.md` | Modify | Catalog the new gauges + clamp signal |
| `internal/controller/plan_test.go` | Modify | Budget-basis cases mirroring existing gate tests (plan_test.go:59-154): clamp at budget; busy-worker up-front subtraction + `allocate()` no-double-count; backstop floor-gate binds / does-not-bind cases proving worker growth alone cannot trip it; hysteresis behavior under a discrete budget term; cross-check clamp; heterogeneous-pool case (per-pool `EffectiveWorker().Memory` — 2GiB default + 4GiB build/review, matching the live 3-pool config from Phase 1) |
| `internal/config/*_test.go` | Modify | Budget validation cases |

TDD: Red-Green-Refactor; new plan_test cases written first against the budget basis.

**Sanity Check:**

- `go build ./...` and `go test ./...` exit 0.
- `grep -c "workerMemoryBudget" internal/config/config.go` ≥ 2 (field + validation).
- New gauge names present in both `internal/telemetry/telemetry.go` and `docs/observability.md`
  (same grep pattern hits both files).
- With a heterogeneous test config (budget per Phase 1 sizing; pools 2GiB default + 4GiB
  build/review), BuildPlan advertises full pool max — asserted in a named test.

**Results (2026-07-18, fresh-context verifier 6/6 PASS):** budget basis shipped —
`workerMemoryBudget` (ByteSize, v2-only, zero=unset=legacy) re-bases slot math to
`budget − Σ active-worker reservations` (busy+starting+idle up front; allocate()
verified no-double-count). Backstop = binary hard floor at the
`MinimumAvailableMemoryPct` reserve gating NEW starts/advertised growth only (held
capacity never withdrawn; no `min(budget, host-headroom)` composition — C1 honored;
does-not-bind case fixtures the Phase 1 measured 17 GiB minimum). Cross-check probe =
Docker Engine `info .MemTotal` (pinned npipe host), once per VM lifecycle, reset on
down observation; config > probe clamps + `worker-memory-budget-exceeds-engine-memory`
problem, probe failure warns + trusts config. Gauges
`ci_runner.capacity.memory.headroom` + `ci_runner.capacity.memory.affordable` and the
`memory-clamped-capacity` log line (log, not problem — legacy clamps must not flip
phase) cataloged in observability docs. Named test
`TestBudgetBasisAdvertisesFullPoolMaxAcrossHeterogeneousPools` proves 8/2/2 full-max
advertisement at 36GiB budget vs 32GiB worst case under a host snapshot that legacy
basis would clamp to 3. Cross-build linux + vet clean. Known-environmental exemption:
`internal/control` named-pipe test fails locally (live controller owns the pipe) —
reproduced identically at baseline 324386c. Phase 3 flag: deployed
`minimumAvailableMemoryPercent` 15 (≈10.2 GB) is a marginal floor once the VM is 40GB —
decide a lower value (e.g. 5) in the Phase 3 config change. Handoff:
`.work/handoffs/2026-07-18-runner-performance-phase-2.md` (memory tier).

### Phase 3: provisioning — WSL2 sizing, budget wiring, drift fix [TODO]

Repo: provisioning (+ operator apply on host). Surface: main session (destructive host steps are
operator-gated). Depends on Phase 2 release (config field must exist before deploy references it).

| File | Action | What changes |
|------|--------|-------------|
| `hosts/melo-desk-001/wslconfig` (net-new surface) | Create | `[wsl2] memory=40GB` (+ explicit swap decision recorded); materialized to `$env:USERPROFILE\.wslconfig` (per-user file, documented location `%UserProfile%\.wslconfig` — the profile of the logon account running Docker Desktop/the controller; write parametrically, never a hardcoded user path) |
| `common/Provisioning.psm1` / `hosts/melo-desk-001/Set-MachineConfiguration.ps1` | Modify | Install/verify `.wslconfig` materialization (net-new management; no template exists today) |
| `hosts/melo-desk-001/ci-runner.yaml` | Modify | Add `workerMemoryBudget` — value derived from MEASURED in-VM MemTotal after the 40GB apply, minus ~4GB daemon/build overhead (removes GB-vs-GiB parsing ambiguity: `.wslconfig` "GB" parsing is not authoritatively documented; kernel MemTotal is ground truth). Add missing `melodic-software-review` pool (repo↔live drift; live config.yaml:63-81) |
| `hosts/melo-lap-001/ci-runner.yaml` | Modify | Same pattern once Phase 1 confirms lap RAM; explicitly sequenced after desk soak |
| `runbooks/melo-desk-001.md` | Modify | Document 3 pools (drift), WSL2 sizing, apply sequence: drain workers → `wsl --shutdown` → Docker Desktop restart → `Install-CiRunnerRelease -ConfigSource …` re-render. Rollback ordering explicit: forward = binary-first (Phase 2 release before config referencing the field); back = config-first (strip `workerMemoryBudget` from deployed config BEFORE any binary downgrade — `KnownFields(true)` decode at config.go:340 crash-loops an old binary on an unknown field). Explicit `.wslconfig` swap value recorded against the per-worker `memorySwap` contract (build/review workers hold 2GiB container swap each; VM swap must back them — do not set swap=0 in isolation). Verify Docker Desktop does not manage/rewrite `[wsl2] memory` and note the VM cap is shared with any other WSL distro use on the box |
| `runbooks/melo-lap-001.md` (or equivalent) | Modify | Lap apply steps |

**Sanity Check:**

- Rendered deployed `config.yaml` contains `workerMemoryBudget` and 3 pools (`grep` on live file
  post-apply).
- `$env:USERPROFILE\.wslconfig` exists with `memory=40GB`; `wsl.exe cat /proc/meminfo` inside the
  VM reports the measured MemTotal that the budget value was derived from.
- Idle-fleet `observed.json` shows advertised MaxCapacity == configured pool max — this is the
  PRIMARY deterministic acceptance for the clamp fix (Phase 7's queue p90 is load-confounded,
  supporting evidence only).
- Busy-window re-sample (same Phase 1 script): advertised stays at pool max under full worker
  load — proves the redesigned backstop does not re-clamp (stress-test C1 regression probe).
- Repo YAML == deployed config modulo documented render sentinels.

### Phase 4: medley checkout — diagnose, then per-lane rollout [TODO]

Repo: medley (locally-owned workflow files only). Surface: main session throughout — the
diagnosis needs live PR babysitting and the rollout is per-lane judgment, NOT mechanical (an
inventory-driven blanket edit would pessimize sparse lanes; see diagnosis below).

**Premise reset (2026-07-17):** lanes are already shallow+blobless (filter and fetch-depth are
independent; default depth 1 applies). The 150-600s cost is NOT history fetch. Leading
hypothesis: lazy on-demand blob fetches during materialization — worst for lanes that materialize
many files (full-tree-gates, non-sparse lint lanes, dotnet's wide sparse cone), amplified by
concurrent clones. The spike diagnoses before any rollout.

Diagnosis spike (throwaway test branch, self-hosted route):

1. Instrument checkout per lane (`GIT_TRACE2_PERF` / checkout step debug): split time into initial
   fetch vs lazy-blob round-trips (count + duration) vs worktree materialization.
2. Per-lane candidate matrix, by lane class:
   - Full-materialization lanes (full-tree-gates, shell-lint bash-tests :82-94, markdown-ci :71-77):
     drop `filter: blob:none` + explicit `fetch-depth: 1` (one packfile of HEAD blobs, zero lazy
     round-trips) vs current.
   - Wide-cone sparse lanes (dotnet-ci :59-67): both shapes measured — cone covers most of the
     monorepo's heavy paths, so either could win.
   - Narrow-cone sparse lanes (python/yaml/markdown lint, single-file aggregator): current shape is
     likely already minimal — measure once to confirm, expect KEEP.
3. Repeat the winning shapes once under fan-out concurrency (full ci-status run) — contention was
   part of the measured signal.

Rollout (evidence-gated, per lane): apply only shapes with measured improvement; a lane that
regresses stays as-is. Route-agnostic: identical inputs under hosted fallback. Scope = ecosystem /
critical-path lanes only (Goal 2 targets medley self-hosted checkout p90) — hosted-only
control-plane workflows are out of scope this phase.

File inventory (tick as processed; ACTION decided by spike, not pre-written):

| File | Action | Rationale |
|------|--------|-----------|
| [ ] `.github/workflows/ci-status.yml` full-tree-gates :283 | SPIKE-DECIDED | full-materialization, prime lazy-fetch suspect |
| [ ] `.github/workflows/dotnet-ci.yml` :59-67, :256, :308 | SPIKE-DECIDED | wide sparse cone |
| [ ] `.github/workflows/shell-lint.yml` :42-53, :82-94, :200-211, :241-253 | SPIKE-DECIDED | mixed sparse + full (bash-tests full) |
| [ ] `.github/workflows/markdown-ci.yml` :30-35, :53-59, :71-77 | SPIKE-DECIDED | mixed; :71-77 full |
| [ ] `.github/workflows/python-ci.yml` 6 checkouts | SPIKE-DECIDED | narrow cones — expect KEEP |
| [ ] `.github/workflows/typescript-ci.yml` :39, :72, :124-138 | SPIKE-DECIDED | narrow cones |
| [ ] `.github/workflows/yaml-ci.yml` :43-45, :68-72 | SPIKE-DECIDED | narrow cones — expect KEEP |
| [ ] `.github/workflows/ci-status.yml` aggregator :492-494 | KEEP | single-file sparse; dropping blob:none would fetch every HEAD blob to check out one script |
| [ ] ci-status `detect-changes` :99-102 | KEEP | merge-base + no-renames contract (ci-status.yml:130-137) |
| [ ] `.github/workflows/secret-scan.yml` :34-37 | KEEP | gitleaks: full history + blobs |
| [ ] Hosted-only control-plane workflows (docs-link-check, label-drift-check, onboard-drift, recurring-issues, tool-version-drift-check, claude-assistant, regen-lockfiles, dotnet-e2e-ci, playwright-visual-ci) | KEEP | off the self-hosted queue and ci-status critical path — out of Goal 2 scope; sparse+blob:none regression risk if blanket-edited |

Deferred (upstream, SHA-pinned contract): `ci-workflows/claude-review.yml:196,237` checkouts —
separate follow-up work item; allowlist lockstep cost ≫ gain.

**Sanity Check:**

- Spike report `.work/runner-performance/verification/checkout-matrix.md` exists: per-lane rows
  (lane, class, lazy-fetch count/time before, candidate shape, after-seconds, verdict).
- Every inventory row ticked with a recorded verdict (MODIFIED with shape, or KEEP with reason).
- `grep -rn "filter: blob:none" .github/workflows/` output matches exactly the post-rollout
  KEEP+unchanged set recorded in the spike report.
- Test-branch full ci-status run green on both routes (self-hosted + forced hosted fallback);
  checkout step p90 across modified lanes < 60s on the test PR.

### Phase 5: medley job consolidation [TODO]

Repo: medley. Surface: main session (judgment per lane). After Phase 4 (attribute gains
separately; separate PRs).

Pre-flight consumer check (FIRST work items — job names are a contract surface):

1. Branch-protection required checks for medley (github-iac source): confirm only the `ci-status`
   aggregate is required, not per-lane job names.
2. `medley/.github/runner-policy.json` exceptions are keyed `.github/workflows/<file>.yml#<jobid>`
   — inventory every key referencing a job this phase merges/renames. Known exposure (verified
   2026-07-17): only `.github/workflows/shell-lint.yml#pester` — at risk iff shell 4→2 renames
   the windows/pester job id; markdown/yaml/python merges touch zero exception keys.
3. `ci-status.yml` aggregator `needs:` list (:484) + per-ecosystem gating `if:` chains.

Ordering note: Phase 4 edits checkouts in jobs Phase 5 then merges (same files — accepted rebase
churn; sequencing is deliberate for gain attribution between filter changes and job-count
changes; where a lane needs no separate attribution, its merge may fold into one PR). Attribution
caveat (stress-test M1): merging jobs widens the surviving job's materialization cone — any lane
whose cone changes in Phase 5 gets its Phase 4 checkout verdict RE-SPIKED, not carried over.

Consolidation targets (full-tree-gates exemplar — one checkout, per-step `if: always()`):
markdown 3→1, yaml 2→1, python 6→2 (static-analysis job + pytest job), shell 4→2 (bash job +
powershell/windows job — windows stays separate: different runner). Keep: typescript
discover→matrix (real parallelism), dotnet build lanes, secret-scan. Expected typical full-run
job count ~25 → ≤16.

**Sanity Check:**

- Jobs-API count for a full-ecosystem test PR run ≤ 16 (excluding skipped sentinels).
- `runner-policy` gate job exits 0 (no orphaned exception keys — exception-inventory-drift rule).
- Aggregator `needs:` list updated; every merged gate still reports as a named step (visible in
  job log); both routes green on test PR.

### Phase 6: side-flag work items [TODO]

Repo: ci-runner issues (provisioning drift is fixed in Phase 3, not filed). Surface:
sub-agent-worker capable. Three items, each with the search-before-create shape:

- [ ] **Phase-entry check per item:** `gh issue list --state all --search '<key-term> in:title'
  --json number,title,state` → match ⇒ `gh issue comment <N>` with the discovery evidence and skip
  create; empty ⇒ create.
- [ ] Scale-set statistics poll failures (×632/24-48h, build/review pools).
- [ ] cgroup terminal-evidence capture failures (×225).
- [ ] OTel historical queryability gap (live-only; blocked the clamp verification from telemetry
  alone).

**Sanity Check:** three issue URLs (created or pivoted-to) recorded in this section.

### Phase 7: re-measure + acceptance [TODO]

After Phases 3-5 deployed. 2-week window including busy periods; same jobs-API sweep method as
baseline; store under `.work/runner-performance/baselines/post-fix/`; record distilled comparison
here (contract carries numbers, not raw output).

Attribution notes (stress-test H2): queue p90 is load-confounded across windows — the
deterministic clamp proof is Phase 3's idle + busy advertised==max checks; Phase 7 shows the
user-visible outcome. Checkout/run p90 criteria are target-based (<60s / <10min) against the
post-Phase-4/5 workflows — the baseline runs' exact job graph no longer exists; stored Phase 1
baseline stats provide the before-picture, not a like-for-like A/B.

**Sanity Check:**

- Comparison table in this file: queue p90, medley checkout p90, medley run p90, success rate —
  each against its Brief target, PASS/FAIL per criterion.
- Worker-invariant criterion explicit: zero invariant-violation problem codes in controller logs
  over the window AND worker-creation code paths untouched by this effort (`git log --stat` of
  merged PRs shows no `internal/worker`/container-creation changes) — nothing in scope modifies
  worker runtime construction.
- Deferred-trigger evaluation recorded: mirror-mount ADR (checkout p90 still >60s?), runtime
  pre-bake (setup dominant?), selector-hop (standalone serial wait material?), capacity raise.

### Alternatives considered

| Alternative | Why rejected |
|-------------|-------------|
| Dynamic WSL2 memory probe as gate basis | Adds cross-VM runtime probe each reconcile + failure modes (WSL down = gate blind); user picked static budget + startup cross-check |
| Mirror mount / repo objects in image / self-hosted cache backend / capacity raise | Ruled out in Brief (posture, staleness, GHES-only, workstation risk) — deferred with triggers |
| Selector-hop removal now | Not on dominant critical path (parallel to detect-changes); inline shapes lose liveness fallback + cost policy lockstep — deferred with trigger |
| `tree:0` (treeless) for medley lanes | No lane besides the KEEP sites needs history; lanes are already depth-1, so treeless would only ADD history cost |
| Blanket "drop blob:none everywhere" | Premise correction killed it: lanes are already shallow; sparse lanes would regress (full HEAD blob fetch to materialize a narrow cone) — per-lane spike-decided instead |
| Consolidating via workflow-level path filters | ci-status deliberately uses data-driven detection (ci-status.yml:23-24); path filters would regress the required-check contract |

### Risks and mitigations

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Host-physical backstop re-clamps under load (vmmem counted in host available — C1) | High if min()'d | High | Backstop redesigned as vmmem-discounted hard floor; Phase 1 measures the coupling; Phase 3 busy-window probe is the regression gate |
| Binary rollback with budget field in deployed config → decode crash-loop (H1) | Low | High | Config-first-back rollback ordering in runbook; forward stays binary-first |
| Budget config drifts from actual VM size | Med | Med | Startup cross-check warns+clamps; provisioning deploys `.wslconfig` + budget together |
| VM swap sizing strands per-worker `memorySwap` contract (M3) | Low | Med | Swap value decided against the 2GiB-per-build/review-worker contract, recorded in runbook |
| 40GB VM squeezes workstation under load | Low | Med | Modest sizing (24GB reserved); revert = delete `.wslconfig` + re-render config |
| Checkout diagnosis finds a cause outside supported checkout inputs (e.g. network/disk contention) | Med | Med | Spike report routes it: contention → Phase 5 consolidation carries more weight; else surface to user before further medley changes |
| depth-1 full-blob fetch regresses sparse lanes | Med | Low | Per-lane spike verdicts; regressing lanes stay as-is |
| Job merges break runner-policy exception keys or required checks | Med | High | Phase 5 pre-flight consumer check FIRST; runner-policy gate must pass pre-merge |
| `wsl --shutdown` during apply kills active workers | Med | Med | Runbook sequence: drain (stop controller) → shutdown → restart; apply in idle window |
| Advertised capacity 12 exposes a different bottleneck (CPU gates) | Low | Low | CPU admission gates unchanged; Phase 7 re-measure catches it |

## Blast radius

**HIGH.** Production CI capacity math (ci-runner controller), host VM memory topology
(workstation impact), and the CI entry path of every private repo (medley workflow surface shared
by hosted fallback). Cross-repo: ci-runner, provisioning, medley (+3 filed issues). No security
posture change; worker invariants untouched.

## Stress-test summary

Two fresh-context passes, findings verified then folded in:

1. Plan-reviewer (Step 3): 2 CRITICAL + 7 IMPORTANT + 4 SUGGESTION. Decisive: actions/checkout
   `filter` and `fetch-depth` are independent — the Brief's "blobless refetches all commits+trees"
   premise was FALSE; Phase 4 rebuilt as diagnose-first, per-lane, main-session (blanket
   mechanical rollout rejected). Sparse KEEP rows added; Phase 1 equality criterion relaxed to
   binding-ceiling; heterogeneous-pool tests; probe re-sequenced after Docker/WSL up + re-probe on
   VM recycle; `.wslconfig` parametric path + budget derived from measured MemTotal; hosted-only
   control-plane lanes descoped; `#pester` exception key named; `monitor_other.go` stub.
2. Devils-advocate (Step 4): 1 CRITICAL + 2 HIGH + 4 MEDIUM, verdict "needs revision" — applied.
   C1: naive `min(budget, host-headroom)` backstop re-introduces the clamp (vmmem host-counted);
   backstop redesigned as vmmem-discounted hard floor, Phase 1 now measures the coupling, Phase 3
   gains a busy-window no-re-clamp probe. H1: rollback = config-first-back (KnownFields decode
   crash-loop). H2: clamp acceptance elevated to Phase 3 deterministic checks; Phase 7 marked
   load-confounded/supporting. M1: re-spike lanes whose cone widens in Phase 5. M2: busy-worker
   subtraction + no-double-count tests. M3: swap-vs-memorySwap coupling recorded. M4: shared-VM +
   Docker Desktop ownership verification item.

No unresolved CRITICAL/HIGH findings remain; no research-iterate round needed beyond the
actions/checkout and `.wslconfig` primary-source verifications already performed.

## Execution shape

Cross-repo dependency chain: Phase 1 → Phase 2 → Phase 3; Phase 4 → Phase 5; Phase 6 free;
Phase 7 last.

| Phase | Files | Overlaps with |
|---|---|---|
| 1 | `.work/` only | none |
| 2 | ci-runner `internal/*`, docs | none |
| 3 | provisioning + live host | none (consumes Phase 2 release) |
| 4 | medley workflows (checkout keys) | 5 (same files, different keys) |
| 5 | medley workflows (job structure) | 4 |
| 6 | GitHub issues | none |
| 7 | `.work/` + this file | none |

> Wave A (after Phase 1, parallel — 2 sessions/worktrees): {Phase 2, Phase 4-spike}
> Wave B: {Phase 3 (after 2), Phase 4-rollout + Phase 5 (after spike, sequential — shared files)}
> Phase 6 any time after Phase 1; Phase 7 after Wave B deployed.
> Cost note: Wave A parallelism = 2 concurrent sessions; modest token multiplier, saves the
> longest serial leg (Go TDD vs spike PR turnaround). Sequential fallback: run 2 → 3 → 4 → 5 in
> order; abort a wave member on scope-fence violation and continue sequentially.

| Phase | Surface | Basis |
|---|---|---|
| 1 | main-session | live host access, verification judgment |
| 2 | main-session | TDD on capacity math — judgment-heavy |
| 3 | main-session | operator-gated destructive host steps |
| 4 (spike + rollout) | main-session | live PR babysitting, measurement, per-lane judgment — NOT mechanical |
| 5 | main-session | per-lane judgment + contract pre-flight |
| 6 | sub-agent worker | mechanical tracker writes with dedup shape |
| 7 | main-session | acceptance judgment |

## Decisions made (gate-passed)

| Decision | What it changes in the plan | Basis (evidence) |
|---|---|---|
| [EXEC-SHAPE] Phase 4 reshaped diagnose-first, per-lane, spike-decided | No blanket filter edit; inventory actions decided by measurement | actions/checkout source: `filter`/`fetch-depth` independent — Brief mechanism claim false; sparse lanes would regress under a blanket edit |
| [EXEC-SHAPE] Phase 4 runs main-session end-to-end | Mechanical sub-agent rollout removed | The rollout IS the judgment the spike produces; fan-out would bake in a pre-written answer |
| [EXEC-SHAPE] Hosted-only control-plane workflows descoped from Phase 4 | 9 workflows become KEEP rows | Off the self-hosted queue and ci-status critical path — outside Brief Goal 2; blanket edits risk regression |
| [EXEC-SHAPE] Backstop = vmmem-discounted hard floor, not `min(budget, host-headroom)`; exact shape from Phase 1 coupling data | Phase 2 design + tests; Phase 3 busy-window probe | Stress-test C1: vmmem is host-counted, both min() terms fall together — naive composition re-introduces the clamp being removed |
| [EXEC-SHAPE] Budget value derived from measured in-VM MemTotal − ~4GB overhead | Phase 3 config value not hardcoded up front | `.wslconfig` "GB" parsing not authoritatively documented; kernel MemTotal is ground truth |
| [EXEC-SHAPE] Consolidation targets: markdown 3→1, yaml 2→1, python 6→2, shell 4→2; typescript matrix + dotnet + secret-scan kept | Phase 5 scope | Within briefed "consolidate toward full-tree-gates"; pre-flight adjusts; only `#pester` exception key exposed |
| [EXEC-SHAPE] `ci-workflows/claude-review.yml` checkouts deferred to a filed work item | Out of Phase 4 | SHA-pinned reusable contract: allowlist lockstep cost ≫ measured gain |
| [EXEC-SHAPE] Wave A parallelism (Phase 2 ‖ Phase 4-spike), worktree-isolated | Execution shape | Disjoint repos/files; saves the longest serial leg |
| [FALLBACK — confirm or override] Phase 1 cannot reproduce clamp → STOP, re-plan from data | Gate before Phase 2 | Brief's correctness-first posture |
| [FALLBACK — confirm or override] Checkout diagnosis lands outside supported checkout inputs → surface before further medley changes | Gate inside Phase 4 | Brief constraint: supported surface only |

## Open questions

- melo-lap-001 RAM + WSL2/budget numbers — resolved by Phase 1 data (sizing pattern locked).
- Exact new gauge names — follow existing `ci_runner.*` conventions at implementation time.

## Handoff to implementation

### User-approval gates

- Phase 3 host apply (workstation-impacting; `wsl --shutdown`): confirm idle window before
  executing.
- Phase 4 rollout proceeds only on spike evidence (per-lane improvement); a lane regressing in
  the matrix stays on `blob:none` — flag it, don't force.
- Phase 5 merges: if branch protection turns out to require per-lane job names (pre-flight
  finding), STOP and surface — required-check changes live in github-iac.
- [FALLBACK — confirm or override] If Phase 1 fails to reproduce the clamp (correlation
  contradicts hypothesis), STOP — re-plan from data; do not proceed to Phase 2 on an unverified
  mechanism.

### Execution shape ([EXEC-SHAPE] tagged)

- [EXEC-SHAPE] Wave A parallelism (Phase 2 ‖ Phase 4-spike) via sibling worktrees/sessions;
  scope fences = per-phase file tables above; PLAN.md edits main-session only.
- [EXEC-SHAPE] Phase 4 stays main-session end-to-end (per-lane spike verdicts drive edits;
  mechanical fan-out rejected after review — it would bake in a blanket answer the spike exists
  to prevent).
- [EXEC-SHAPE] Consolidation targets (markdown 3→1, yaml 2→1, python 6→2, shell 4→2; typescript
  matrix + dotnet + secret-scan kept) — discretion within the briefed "consolidate toward
  full-tree-gates" scope; adjust from pre-flight findings.
- [EXEC-SHAPE] Budget value = measured in-VM MemTotal (post-40GB apply) − ~4GB daemon/build
  overhead; validated by the Phase 2 cross-check and Phase 3 sanity checks. Worst-case
  3-pool reservation sum (Phase 1) must fit under it, else surface before apply.
- [EXEC-SHAPE] claude-review.yml (ci-workflows) checkout change deferred to a filed work item
  (allowlist lockstep cost ≫ gain).

### Mechanical work

- One PR per repo per phase; PLAN.md phase-tag updates ride the same commit as that phase's
  changes (branch contract tier). Commit subjects per each repo's own convention.
- Verification checkpoints: each phase's Sanity Check block executes before its PR merges;
  Phase 7 closes the topic via `/planning:architect close-out`.
- Sequential fallback documented in Execution shape.
