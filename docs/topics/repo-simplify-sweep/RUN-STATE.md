# batch-simplify repo-mode run state

Branch: `claude/repo-code-tidying-simplify-z75kiv`. Universe: 244 files; 210 survive
Phase 2 filters; all tracked, working tree clean at start (no untracked snapshots
needed — group reverts are `git checkout -- <group files>`).

Authorization note: the repo-mode confirmation gate was passed on the user's
up-front instruction to run the whole-repository sweep autonomously end to end
("spend as much time as you need to get this done right"); this session is
unattended, so the inventory summary was recorded here rather than interactively
confirmed. Recorded for the Phase 8 report.

Delivery: waves land as sequential commits on the designated branch (no PRs —
session harness constraint). Repo-mode PR-per-wave delivery deviation recorded.

## Exclusions (34 files)

- markdown (15): docs/**, README.md, AGENTS.md, .claude/source-control.md — `docs` flag not set
- CI enforcement config (10): .github/workflows/*.yml, .github/dependabot.yml
- agent config / synced (3): .claude/settings.json, .claude/cloud-bootstrap.sh
  (self-declared sync-materialized from upstream SSOT — read-only deferred class),
  .work-item-tracker.json
- data records (3): release/dependencies.json, release/dependency-drift-review.json,
  release/compatibility.schema.json
- fixtures (2): scripts/fixtures/production-cmd-harness/*
- generated lock (1): go.sum

## Canonical clusters

None in-repo (content-hash + generated-header scan). `.editorconfig-checker.json`
and `.gitleaks.toml` are the AUTHORED canonical copies of policy consumed by other
repos — simplify conservatively, never restructure documented contracts.
Platform `_windows.go`/`_other.go` pairs are distinct authored implementations,
not copies.

## Groups (20) and status

Statuses: pending | in-flight | simplified | verified | delivered | deferred | failed.
File lists: deterministic mapping mirrored below from the refined grouping
(scratchpad/refined.tsv in-session); groups are file-disjoint.

| Grp | Name | Files | Wave | Status |
|---|---|---|---|---|
| G1 | root-config | 4 (9 deferred read-only) | 1 | verified (no changes) |
| G2 | ci-scripts | 10 | 1 | delivered (wave 1) |
| G3 | powershell | 3 | 1 | delivered (wave 1; refuted hunk reverted, safe subset re-applied) |
| G4 | worker-image | 8 | 1 | delivered (wave 1) |
| G5 | go-shared-small | 9 | 1 | delivered (wave 1) |
| G6 | go-scaleset | 5 | 1 | delivered (wave 1) |
| G7 | go-telemetry | 6 | 2 | delivered (wave 2) |
| G8 | go-state | 14 | 2 | delivered (wave 2) |
| G9 | go-control | 8 | 2 | delivered (wave 2; refuter note — WaitGroup.Go skips Done on panic unwind, observable only during process death, accepted) |
| G10 | go-secret | 15 | 2 | delivered (wave 2) |
| G11 | go-jobindex | 8 | 3 | delivered (wave 3) |
| G12 | go-controller-src | 9 | 3 | delivered (wave 3) |
| G13 | go-controller-tests | 11 | 3 | delivered (wave 3; one unreproduced full-suite flake disclosed by refuter — 42+ clean runs since incl. 4 orchestrator race-double-runs; differential baseline showed no stability difference) |
| G14 | go-healthwatch | 7 | 3 | delivered (wave 3) |
| G15 | go-host-core | 19 | 4 | delivered (wave 4) |
| G16 | go-host-probes | 10 | 4 | delivered (wave 4) |
| G17 | go-host-tests | 8 | 4 | delivered (wave 4) |
| G18 | go-runtime-docker | 13 | 4 | delivered (wave 4) |
| G19 | go-app-src | 23 | 5 | pending |
| G20 | go-app-tests | 11 | 5 (after G19) | pending |

### Group file lists

- G1 root-config (NARROWED post-grounding): .dockerignore .github/actionlint.yaml
  .gitignore go.mod — the other nine base-pass members (.editorconfig,
  .editorconfig-checker.json, .gitattributes, .gitleaks.toml, .golangci.yml,
  .markdownlint-cli2.jsonc, .shellcheckrc, _typos.toml, lychee.toml) are
  standards-synced copies (git history: every touch is a "chore: sync standards
  components" / "adopt shared standards" commit; upstream owner
  melodic-software/standards). Read-only deferred class per repo-mode
  externally-managed rule; recorded as deferred items, not swept.
- G2 ci-scripts: .github/scripts/budget-monitor.cjs
  .github/scripts/budget-monitor.test.cjs .github/scripts/incident-issue.cjs
  .github/scripts/queue-monitor.cjs .github/scripts/queue-monitor.test.cjs
  .github/scripts/release-transaction.cjs
  .github/scripts/release-transaction.test.cjs
  .github/scripts/verify-existing-release.sh
  .github/scripts/verify-existing-release.test.cjs
  .github/scripts/workflow-pin-metadata.test.cjs
- G3 powershell: scripts/New-CompatibilityManifest.ps1
  scripts/Test-DependencyFreshness.ps1 scripts/Test-ReleasePins.ps1
- G4 worker-image: Dockerfile worker/capture-cgroup.sh worker/entrypoint.sh
  worker/job-completed.sh worker/job-started.sh worker/set-state.sh
  scripts/install-verified-buildx.sh scripts/verify-worker-image.sh
- G5 go-shared-small: internal/buildinfo/buildinfo.go
  internal/childprocess/command.go internal/childprocess/command_other.go
  internal/childprocess/command_windows.go
  internal/childprocess/command_windows_test.go internal/clock/clock.go
  internal/config/config.go internal/config/config_test.go internal/model/model.go
- G6 go-scaleset: internal/scaleset/fake.go internal/scaleset/official.go
  internal/scaleset/official_test.go internal/scaleset/scaleset.go
  internal/scaleset/scaleset_test.go
- G7 go-telemetry: internal/telemetry/export.go internal/telemetry/export_degraded.go
  internal/telemetry/export_degraded_test.go internal/telemetry/export_test.go
  internal/telemetry/telemetry.go internal/telemetry/telemetry_test.go
- G8 go-state: internal/state/store.go internal/state/store_test.go
  internal/state/fs/lockwait.go internal/state/fs/lockwait_test.go
  internal/state/fs/lockwait_windows_test.go internal/state/fs/mutex_other.go
  internal/state/fs/mutex_windows.go internal/state/fs/mutex_windows_test.go
  internal/state/fs/replace_other.go internal/state/fs/replace_windows.go
  internal/state/fs/store.go internal/state/fs/store_open_block_unix_test.go
  internal/state/fs/store_open_block_windows_test.go internal/state/fs/store_test.go
- G9 go-control: internal/control/pipe_other.go internal/control/pipe_windows.go
  internal/control/pipe_windows_test.go internal/control/protocol.go
  internal/control/protocol_test.go internal/control/transport.go
  internal/control/transport_fuzz_test.go internal/control/transport_test.go
- G10 go-secret: internal/secret/acl_other.go internal/secret/acl_windows.go
  internal/secret/bitlocker.go internal/secret/bitlocker_other.go
  internal/secret/bitlocker_windows.go internal/secret/bitlocker_windows_test.go
  internal/secret/dpapi_other.go internal/secret/dpapi_windows.go
  internal/secret/dpapi_windows_test.go internal/secret/secret.go
  internal/secret/secret_test.go internal/secret/source.go
  internal/secret/source_other.go internal/secret/source_windows.go
  internal/secret/source_windows_test.go
- G11 go-jobindex: internal/jobindex/assign_times.go internal/jobindex/catalog.go
  internal/jobindex/catalog_test.go internal/jobindex/compactable_test.go
  internal/jobindex/drop_journal.go internal/jobindex/drop_journal_test.go
  internal/jobindex/file_store.go internal/jobindex/snapshot_bytes_test.go
- G12 go-controller-src: internal/controller/capacity_transition.go
  internal/controller/control_handler.go internal/controller/force_stop.go
  internal/controller/plan.go internal/controller/poll_cadence.go
  internal/controller/ports.go internal/controller/reconciler.go
  internal/controller/retry.go internal/controller/shutdown.go
- G13 go-controller-tests: internal/controller/capacity_transition_test.go
  internal/controller/control_handler_test.go internal/controller/force_stop_test.go
  internal/controller/handshake_recovery_test.go internal/controller/plan_test.go
  internal/controller/poll_cadence_test.go
  internal/controller/reconcile_watchdog_test.go
  internal/controller/reconciler_test.go internal/controller/retry_test.go
  internal/controller/shutdown_test.go internal/controller/start_before_poll_test.go
- G14 go-healthwatch: internal/healthwatch/alert.go internal/healthwatch/alert_test.go
  internal/healthwatch/check.go internal/healthwatch/check_test.go
  internal/healthwatch/inventory.go internal/healthwatch/storage.go
  internal/healthwatch/storage_test.go
- G15 go-host-core: internal/host/command.go internal/host/types.go
  internal/host/platform_other.go internal/host/trusted_tools_windows.go
  internal/host/controller_desktop.go internal/host/controller_desktop_other.go
  internal/host/controller_desktop_windows.go internal/host/controller_process.go
  internal/host/controller_process_other.go
  internal/host/controller_process_windows.go internal/host/monitor_other.go
  internal/host/monitor_windows.go internal/host/engine_memory_other.go
  internal/host/engine_memory_windows.go internal/host/healthwatch_task.go
  internal/host/healthwatch_task_other.go internal/host/healthwatch_task_windows.go
  internal/host/gaming.go internal/host/json_log.go
- G16 go-host-probes: internal/host/docker_parse.go internal/host/docker_windows.go
  internal/host/wsl_parse.go internal/host/wsl_windows.go
  internal/host/pendingreboot.go internal/host/pendingreboot_other.go
  internal/host/pendingreboot_windows.go internal/host/reboot.go
  internal/host/reboot_other.go internal/host/reboot_windows.go
- G17 go-host-tests: internal/host/controller_process_windows_test.go
  internal/host/docker_windows_test.go internal/host/gaming_test.go
  internal/host/host_test.go internal/host/json_log_test.go
  internal/host/pendingreboot_test.go internal/host/pendingreboot_windows_test.go
  internal/host/reboot_args_test.go
- G18 go-runtime-docker: internal/runtime/docker/artifacts.go
  internal/runtime/docker/artifacts_audit.go
  internal/runtime/docker/artifacts_audit_test.go
  internal/runtime/docker/artifacts_test.go internal/runtime/docker/endpoint_other.go
  internal/runtime/docker/endpoint_windows.go internal/runtime/docker/log_evidence.go
  internal/runtime/docker/log_evidence_test.go
  internal/runtime/docker/resource_evidence.go
  internal/runtime/docker/resource_evidence_test.go internal/runtime/docker/runtime.go
  internal/runtime/docker/runtime_fuzz_test.go internal/runtime/docker/runtime_test.go
- G19 go-app-src: cmd/ci-runner/main.go cmd/ci-runner-controller/main.go
  cmd/ci-runner-controller/main_test.go internal/app/app.go internal/app/bootstrap.go
  internal/app/compatibility.go internal/app/controller_main.go
  internal/app/controller_restart.go internal/app/doctor.go
  internal/app/doctor_inspector.go internal/app/force_stop.go
  internal/app/health_watch.go internal/app/health_watch_checker.go
  internal/app/logs.go internal/app/logs_artifacts.go internal/app/menu.go
  internal/app/output.go internal/app/path_security.go
  internal/app/path_security_other.go internal/app/path_security_windows.go
  internal/app/reboot.go internal/app/release_command.go
  internal/app/worker_runtime.go
- G20 go-app-tests: internal/app/app_test.go internal/app/compatibility_test.go
  internal/app/controller_main_restart_test.go
  internal/app/controller_main_watchdog_test.go internal/app/doctor_inspector_test.go
  internal/app/doctor_test.go internal/app/health_watch_install_test.go
  internal/app/health_watch_test.go internal/app/logs_test.go
  internal/app/path_security_test.go internal/app/release_command_test.go

## Baselines (pre-change, all PASS unless noted)

- `go build ./...`, `go vet ./...`: PASS
- `go test -race ./...`: PASS except one pre-existing ENVIRONMENTAL failure —
  `internal/state/fs TestLoadObservedTransientFailuresAreNotCorrupt/open_failure_on_valid_file`
  injects an open failure via `chmod 000`, which cannot block a root process
  (this container runs uid 0; CI's non-root runner passes). Deterministic here;
  not a repo defect; wave verification compares against this baseline.
- `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...` + `go vet ./...`: PASS
- `golangci-lint run ./...` (v2.6.0 built per AGENTS.md): PASS
- `node --test .github/scripts/*.test.cjs`: PASS (97/97)
- `pwsh ./scripts/Test-ReleasePins.ps1`: PASS
- `shellcheck --rcfile=.shellcheckrc worker/*.sh scripts/*.sh` (v0.11.0 = CI pin): PASS
- `shfmt -d worker/*.sh scripts/*.sh` (v3.14.0; CI pins 3.13.1): PASS

## Grounding decisions (open questions resolved by orchestrator; unattended run)

1. `internal/state/fs` open-failure subtest: known-red environmental baseline
   (uid 0); simplifiers neither claim nor fix it.
2. Comment reductions: OUT of scope except provably stale comments (comments
   encode security rationale and reviewed decisions; comment-hygiene CI lane
   grades them; never add TODO/FIXME/HACK/XXX markers).
3. golangci-lint v2.6.0 built from source before wave 1: done (baseline PASS).
4. `internal/app/app.go` usage() stray tab: leave — usage output bytes are
   contract.
5. `modernize` linter NOT enabled in .golangci.yml (file is standards-synced +
   claim only MEDIUM confidence); modernize-style rewrites applied only where a
   simplifier verifies them individually.
6. Semantic-fix classes (PowerShell $null-on-left flips, bash pipe-to-while →
   readarray with outer-state mutation, process.exit → process.exitCode): NOT
   applied — behavior-preserving sweep; recorded as deferred items where found.
7. `util.styleText`: not introduced.

## Deferred items (verbatim, keyed by group)

- G1/root-config (orchestrator, pre-dispatch): nine standards-synced root
  configs (.editorconfig, .editorconfig-checker.json, .gitattributes,
  .gitleaks.toml, .golangci.yml, .markdownlint-cli2.jsonc, .shellcheckrc,
  _typos.toml, lychee.toml) are externally managed by melodic-software/standards
  sync; any simplification belongs upstream. Reason: local edits are overwritten
  on next sync. Scope: n/a. Category: cleanup (upstream).
- Excluded-class (orchestrator, pre-dispatch): .claude/cloud-bootstrap.sh is
  sync-materialized (distribution/sync-manifest.yml); improvements belong
  upstream. Reason: externally managed. Scope: n/a. Category: cleanup (upstream).

- G1: .dockerignore:3-4 — `!worker/` and `!worker/*.sh` are mutually redundant
  today (worker/ contains only .sh files); one line could go. Reason:
  negation/traversal semantics differ between classic builder and BuildKit
  patternmatcher and diverge again if a non-.sh file is ever added to worker/;
  equivalence is only provable by building the image, which the local
  environment cannot do (worker-image lane is CI-only). Scope: trivial.
  Category: cleanup.
- G1: go.mod:22-56 — the second and third `require` blocks could be merged into
  the canonical two-block layout (`go mod tidy` normal form), keeping the GHSA
  comment attached to its otelhttp line. Reason: go.mod edits are explicitly
  owned by dependabot/in-flight branches per dispatch; `go mod tidy` is
  forbidden; parallel-edit conflict risk. Scope: small. Category: cleanup.

- G2: .github/scripts/budget-monitor.cjs:228-243 + queue-monitor.cjs:207-227 —
  close-incident sequence duplicated across both monitors, could live in
  incident-issue.cjs. Reason: recovery/info prose differs per monitor (tests
  assert it); module.exports shape is contract. Scope: medium. Category: dedup.
- G2: release-transaction.cjs:345-350 — verify-tag branch re-implements a subset
  of validateInput with deliberately different error strings. Reason: dedup
  would change error text (contract). Scope: small. Category: dedup.
- G2: verify-existing-release.sh:31-32,45-46 — runtime LC_ALL=C sort of
  literal arrays could be pre-sorted literals. Reason: the sort is
  self-maintaining against future edits. Scope: trivial. Category: cleanup.
- G2: verify-existing-release.sh:38 — download loop could iterate expected[@].
  Reason: would reorder gh invocations; SHA256SUMS order is asserted by the
  behavioral test. Scope: trivial. Category: cleanup.
- G2: budget-monitor.test.cjs:29-30 + queue-monitor.test.cjs:24-25 —
  ISSUE_AUTHOR_LOGIN/ownIssue helpers duplicated across test files. Reason:
  shared helper needs a new file (cross-file, out of scope). Scope: small.
  Category: dedup.
- G2: queue-monitor.cjs:122 — healthy-summary prose hardcodes "five minutes"
  while QUEUE_THRESHOLD_MINUTES is configurable. Reason: rendered prose change
  is not behavior-preserving. Scope: trivial. Category: cleanup.
- G3 (REFUTED, do not re-attempt): Test-ReleasePins.ps1:13,15 —
  `[regex]::Replace($x,'\s+',' ')` → `-replace` swap. Refutation: zero-byte
  README.md/docs/releases.md makes Get-Content -Raw return AutomationNull; the
  two forms throw different exceptions in the failure path. A null-guard would
  also change behavior. Category: judgment-call preserved (do-not-file).
- G3: Test-DependencyFreshness.ps1:267-277 — syft image inspection near-copies
  Get-OfficialImageIndexDigest. Reason: dedup changes throw-message prose;
  script cannot be executed to verify. Scope: small. Category: dedup.
- G3: Test-DependencyFreshness.ps1:388-398 — runner image inspection, same
  near-copy. Reason: same prose-contract concern. Scope: small. Category: dedup.
- G3: Test-DependencyFreshness.ps1:150,165,185,240,256,263,315,338 — driftDate
  fallback repeats 8 times; helper or ?? would dedupe. Reason: ?? differs on
  non-null falsy edges; unrunnable script. Scope: medium. Category: dedup.
- G3: Test-DependencyFreshness.ps1:191 — simplified Where-Object syntax
  inconsistent with three sibling scriptblock lookups. Reason: StrictMode
  behavior differs on missing property; not provably identical. Scope: trivial.
  Category: cleanup.
- G3: Test-DependencyFreshness.ps1:96-103 — Add-Drift [bool] $Critical vs house
  [switch]. Reason: positional bool call at :151 breaks; unrunnable. Scope:
  small. Category: modernize.
- G3: Test-DependencyFreshness.ps1 (whole file) — positional → named parameters
  on ~25 internal calls. Reason: parse-level verification only; risk outweighs
  benefit. Scope: medium. Category: modernize.
- G3: Test-ReleasePins.ps1:189-194 — single-element foreach could be plain if.
  Reason: matches eight sibling assertion loops; churn. Scope: trivial.
  Category: cleanup.
- G3: Test-ReleasePins.ps1:95-101,138-149,154-163 — three once-used named
  arrays could be inlined. Reason: style churn touching PR-blocking contract
  strings. Scope: trivial. Category: cleanup.
- G3: New-CompatibilityManifest.ps1:100-103 + Test-DependencyFreshness.ps1:
  409-412 — identical output-directory-creation blocks; shared helper needs a
  new file. Reason: cross-file refactor out of scope. Scope: small.
  Category: dedup.
- G4: verify-worker-image.sh:30-31 — hook-env assertions duplicate image_env
  select logic. Reason: image_env adds an exactly-one jq error, changing
  failure-path behavior. Scope: trivial. Category: dedup.
- G4: verify-worker-image.sh:253-254,292-293 — fixture_script/unavailable_script
  pre-inits are dead (read always assigns). Reason: defensive idiom; set -u
  interplay debate; zero win. Scope: trivial. Category: cleanup.
- G4: capture-cgroup.sh:19-46 — read_scalar/read_stat share identical
  validate/missing tail. Reason: indirection for ~6 lines in load-bearing
  degrade paths. Scope: small. Category: dedup.
- G4: capture-cgroup.sh:113 — printf|jq --raw-input could be jq --args form.
  Reason: telemetry-path JSON reshape needs in-image jq re-verification.
  Scope: trivial. Category: modernize.
- G5: childprocess/command_windows_test.go:25 — reflect.DeepEqual on []string
  could be slices.Equal. Reason: DeepEqual is the repo test convention (12
  uses); windows-only file, compile-check-only confidence. Scope: trivial.
  Category: modernize.
- G5: config.go:437-440,449-451 — defensive nil checks after
  dereferenceYAMLNode are unreachable. Reason: safety margin depends on yaml.v3
  internals; checks cost nothing. Scope: trivial. Category: cleanup.
- G5: clock.go:20-27 — NewTimer/defer Stop/select could be time.After (leak-free
  since Go 1.23). Reason: style-neutral; be-conservative caution. Scope:
  trivial. Category: modernize.
- G5: config_test.go:444-460 — HasPrefix-branched replacements could be a
  {old,new} table. Reason: pure churn in an asserting test. Scope: small.
  Category: refactor.
- G6: official.go:551 — sameLabels join comparison could be slices.Equal.
  Reason: NOT strictly behavior-preserving (\x00/\x01 collision edge changes
  UpdateRunnerScaleSet call decision). Scope: trivial. Category: cleanup.
- G6: scaleset.go:132-139 + official.go:583 — errors.As → errors.AsType[T]
  (Go 1.26 modernize). Reason: errors.As is the repo-wide convention (8+
  packages owned by other groups); belongs in a coordinated pass. Scope: small.
  Category: modernize.
- G6: official.go:186-190 — stale byScaleSet cleanup loop could be
  maps.DeleteFunc. Reason: equal length, no clarity gain. Scope: trivial.
  Category: modernize.
- G6: fake.go:79 — Sprintf("listener-%s") → concat. Reason: breaks symmetry
  with adjacent Sprintf. Scope: trivial. Category: cleanup.
- G6: official_test.go:747,836 — locals named `copy` shadow the builtin.
  Reason: cosmetic; lint-clean at baseline. Scope: trivial. Category: cleanup.

- G7: export.go:182-210 — NewFromEnv re-derives probe endpoint/protocol already
  consulted earlier; could be captured once during enable phase. Reason:
  degraded-mode startup-probe semantics must stay byte-for-byte; equivalence
  hard to prove locally. Scope: small. Category: dedup.
- G7: export.go:83-89 — Shutdown reverse loop could use slices.Backward.
  Reason: not clearly simpler; churn. Scope: trivial. Category: modernize.
- G7: telemetry.go:387-398 — RecordCapacityCheckpoint ignores heartbeatAt
  (frozen signature); single-pool helper restructure neutral. Reason: frozen
  exported shape; neutral size. Scope: trivial. Category: refactor.
- G8: store.go:99-101 — append([]model.X(nil), ...) → slices.Clone. Reason:
  nil-ness differs for empty non-nil input (appendclipped hazard). Scope:
  trivial. Category: modernize.
- G8: store_test.go:40-46 — Add/Done → sync.WaitGroup.Go. Reason: repo-wide
  convention decision (five Add/Done sites elsewhere). Scope: trivial.
  Category: modernize. NOTE: G9 applied wg.Go in transport.go the same wave —
  consolidation should reconcile repo-wide (see 6.5).
- G8: fs/mutex_windows.go:20-22 — local wait consts duplicate windows.WAIT_*.
  Reason: cosmetic dedup in correctness-critical mutex code. Scope: trivial.
  Category: cleanup.
- G8: fs/store_test.go:30-32 — lockFailureLocker nil-err default branch dead.
  Reason: defensive test-double scaffolding. Scope: trivial. Category: cleanup.
- G9: transport.go:277,287,292 — append(nil, x...) → slices.Clone. Reason:
  appendclipped disabled-by-default; nil-ness hazard. Scope: trivial.
  Category: modernize.
- G9: transport.go:361-367 — newRequestID rand.Read error branch unreachable
  per Go 1.24+ docs; dropping simplifies 4 call sites. Reason: removes
  defensive error plumbing in wire client on docs guarantee alone. Scope:
  small. Category: cleanup.
- G9: transport.go:123-148 vs 324-340 — server readRequest / client
  roundTripMessage duplicate framing sequence. Reason: error texts deliberately
  diverge; EOF semantics differ; parameterization changes contractual strings.
  Scope: medium. Category: dedup.
- G9: pipe_windows.go:20 — no-verb fmt.Errorf → errors.New. Reason: agent
  judged fmt.Errorf the dominant convention; cross-group decision. NOTE:
  G10 converted its sites the same wave — consolidation should reconcile
  (see 6.5). Scope: trivial. Category: cleanup.
- G9: pipe_windows_test.go:28-45 — four cancel() calls could be one defer.
  Reason: windows-only test, compile-verified only. Scope: trivial.
  Category: cleanup.
- G10: secret.go:465-469 — zero() loop could be clear() builtin. Reason:
  zeroization-pattern caution (audited form). Scope: trivial.
  Category: modernize.
- G10: secret.go:490-498 — clearBigInt bits loop could use clear(bits).
  Reason: same. Scope: trivial. Category: modernize.
- G10: dpapi_windows.go:72-76 — unmanaged-buffer wipe loop could use clear().
  Reason: same + windows-only. Scope: trivial. Category: modernize.
- G10: secret.go:374-393 — fingerprint helpers share marshal+SHA-256 prefix.
  Reason: security-critical path, marginal 4-line dedup. Scope: small.
  Category: dedup.

- G11: assign_times.go:38-63 + drop_journal.go:58-83 — full dedup of the two
  sidecar loaders into one generic function. Reason: per-step error strings
  diverge inconsistently, forcing string plumbing that adds indirection; only
  the decode core was safely shareable. Scope: medium. Category: dedup.
- G11: assign_times.go:152-193 + drop_journal.go:128-166 + file_store.go:
  310-358 — dedup the triplicated temp-file write→chmod→sync→close→ACL→
  atomic-replace sequence. Reason: the three differ materially (error
  swallowing per livelock comment, ACL ordering, distinct error strings) —
  high refutation risk. Scope: large. Category: dedup.
- G11: catalog.go:156-160 — unroll map-range in Validate into two explicit
  checks. Reason: would make the both-invalid error message deterministic —
  an observable narrowing. Scope: trivial. Category: cleanup.
- G11: file_store.go:101-131 — FindByJobID/FindByRunner near-duplicate scans
  could share a predicate scan. Reason: indirection for ~8 lines. Scope:
  small. Category: refactor.
- G11: drop_journal_test.go:186 — os.IsNotExist → errors.Is(os.ErrNotExist).
  Reason: repo convention mixed; repo-wide decision. Scope: trivial.
  Category: modernize.
- G11: catalog.go:180-187, assign_times.go:118-124, file_store.go:427-437,
  487-497 — sort.Slice → slices.SortFunc. Reason: not endorsed for struct
  comparators; rewrite risk, no clarity gain. Scope: small.
  Category: modernize.
- G11: assign_times.go:187-188 + file_store.go saveUnlocked — "verify ... ACL"
  error text on a Harden failure (copy-paste inconsistency). Reason: fixing
  changes error strings (forbidden). Scope: trivial. Category: cleanup.
- G12: reconciler.go:590-601 — polls Add/go/Done → WaitGroup.Go. Reason:
  requires switching from pass-snapshots-as-arguments convention to closure
  capture; structural change in reconciler-core concurrency. Scope: small.
  Category: modernize.
- G12: force_stop.go:75,86-87 — sort.Slice → slices.SortFunc. Reason:
  duplicate-WorkerID tie order is observable preview output. Scope: trivial.
  Category: modernize.
- G12: plan.go:638-647 — append(nil,...) → slices.Clone and sort.SliceStable →
  SortStableFunc. Reason: appendclipped forbidden; SortStableFunc not
  endorsed. Scope: trivial. Category: modernize.
- G12: plan.go:680-702,834-838 + poll_cadence.go:85-88 + reconciler.go:
  992-996 — `value := now; ptr = &value` copies could be &now. Reason:
  aliasing subtle, copy arguably deliberate. Scope: trivial.
  Category: cleanup.
- G12: plan.go:783-800 — hostMemoryAtFloor/availableMemoryHeadroom duplicate
  validity+reserved-bytes computation. Reason: invalid-observation results
  differ (false vs 0); indirection in highest-risk admission math. Scope:
  small. Category: dedup.
- G12: poll_cadence.go:355 — `return len(workers) == 0` provably always true.
  Reason: defensive check, removal rests on subtle proof. Scope: trivial.
  Category: cleanup.
- G12: control_handler.go:148-166 — mirror to/from force-stop converters.
  Reason: distinct struct types make a generic helper no simpler. Scope:
  small. Category: dedup.
- G12: retry.go:44 — `for i := 1; i < attempt; i++` (i unused) → range form.
  Reason: obscures "attempts after the first". Scope: trivial.
  Category: modernize.
- G13: control_handler_test.go:171-222 — two force-stop tests duplicate a
  draining fixture; could share a NEW helper. Reason: new fixture helper adds
  indirection for 2 sites in safety-net tests. Scope: small. Category: dedup.
- G13: control_handler_test.go:108,182,209,234,252 — NewControlHandler error
  inconsistently discarded vs checked. Reason: either direction changes
  assertion surface. Scope: trivial. Category: cleanup.
- G13: reconciler_test.go:2603 — missing t.Parallel() on one test. Reason:
  changes scheduling the test itself observes. Scope: trivial.
  Category: cleanup.
- G13: reconciler_test.go:2628-2632 — testLogSink.String() declared away from
  its type. Reason: churn-only move. Scope: trivial. Category: cleanup.
- G14: alert.go:19-29 — collapse DefaultClient nil-check/clone to plain
  &http.Client{Timeout: 15s}. Reason: current code inherits operator-installed
  Transport/proxy from the mutable global at construction — collapse changes
  observable behavior under global mutation. Scope: small. Category: cleanup.
- G14: alert.go:118 — errors.Join → fmt.Errorf %w style alignment. Reason:
  rendering differs ("\n" vs ": ") — error bytes change. Scope: trivial.
  Category: cleanup.
- G14: check.go:204-207 — `started := now; ... = &started` → &now. Reason:
  named copy documents intent; nil gain. Scope: trivial. Category: cleanup.

- G15: controller_desktop.go:106-114 + monitor_windows.go:96-104 — drop the
  pre-Go-1.23 timer drain (`if !timer.Stop() { <-timer.C }`). Reason: post-Stop
  receive semantics changed in Go 1.23; cannot prove the edge identical from
  local evidence; timers touch concurrency. Scope: trivial. Category: modernize.
- G15: monitor_windows.go:18 — syscall.NewLazyDLL → windows.NewLazySystemDLL
  (hardening). Reason: changes DLL search path — a fix, not a simplification.
  Scope: trivial. Category: cleanup.
- G15: controller_process_windows.go:13-19 — local process/wait constants
  duplicate windows.* constants. Reason: windows-only, compile-check-only net.
  Scope: trivial. Category: dedup.
- G15: trusted_tools_windows.go:35 — ContainsAny redundant with filepath.Base
  under windows. Reason: security-frozen defense-in-depth kept. Scope: trivial.
  Category: cleanup.
- G15: controller_desktop.go:22,39,54 — hoist thrice-repeated errors.New to a
  package var. Reason: changes error identity (==). Scope: trivial.
  Category: dedup.
- G15: controller_desktop.go:38-66 — merge Start/Stop near-duplicates into a
  5-parameter helper. Reason: indirection without clear simplification.
  Scope: small. Category: refactor.
- G15: json_log.go:347-352 — sort.Slice → slices.SortFunc. Reason: mixed repo
  convention; comparator rewrite risk. Scope: trivial. Category: modernize.
- G15: json_log.go:283,313 — drop redundant .UTC() in rotateLocked. Reason:
  defensive normalization guards future callers/test-injected now. Scope:
  trivial. Category: cleanup.
- G16: wsl_windows.go:16 + reboot_windows.go:23-25 — identical one-line
  trustedSystemExecutable closures could curry via a helper. Reason: natural
  home is G15's frozen security file; relocation isn't simplification.
  Scope: trivial. Category: dedup.
- G17: json_log_test.go:58-72,101-112 — shared readCombinedLogs helper for
  duplicated fixture. Reason: new-helper refactor beyond wave scope. Scope:
  small. Category: dedup.
- G17: docker_windows_test.go:63-67,112-116 — duplicated ExitError prerequisite
  block. Reason: windows-only, compile-check-only. Scope: small.
  Category: dedup.
- G17: gaming_test.go:56-60,124,201 — answeringDesktop subset of fakeDesktop.
  Reason: naming vocabulary is deliberate documentation. Scope: small.
  Category: dedup.
- G17: reboot_args_test.go:15-19 — /d-before-/c index assertion redundant after
  slices.Equal. Reason: assertions immovable; documents intent. Scope: trivial.
  Category: cleanup.
- G17: host_test.go:46 + controller_process_windows_test.go:21 —
  reflect.DeepEqual on []string → slices.Equal. Reason: mixed precedent;
  windows file compile-check-only. Scope: trivial. Category: modernize.
- G18: runtime_test.go:2127-2133 — cloneLabels → maps.Clone. Reason: nil-map
  nilness differs (mapsloop conservatism). Scope: trivial. Category: modernize.
- G18: artifacts.go:96-268 — OpenLog/WriteDiagnostics/WriteResourceEvidence
  share a temp-write/harden/replace skeleton. Reason: per-variant error prose
  is operator contract; extraction risks drift. Scope: medium. Category: dedup.
- G18: artifacts.go:515-582 — artifactDiskTotal/protectedCountedBytes duplicate
  a scan loop. Reason: shared iterator changes error-surfacing points. Scope:
  small-medium. Category: dedup.
- G18: log_evidence.go:72-79 — local named `copy` shadows builtin. Reason:
  rename churn. Scope: trivial. Category: cleanup.
- G18: artifacts_audit.go:169-178 — redundant zero-value assignment + 3-way
  branch compression. Reason: explicit three-state logic documents semantics.
  Scope: trivial. Category: cleanup.

(further items populated as waves complete)

## Wave delivery log

- Wave 1 (G1-G6): delivered as one code commit on the designated branch.
  9 files changed across G2/G3/G4/G5/G6; G1 reviewed no-change. All five
  changed groups refutation-verified NOT REFUTED (G3 after one confirmed
  refutation → group revert → safe-subset re-run). Wave verification: go
  build/vet, GOOS=windows build/vet, go test -race (config, scaleset),
  golangci-lint 0 issues, node --test 97/97, Test-ReleasePins PASS,
  shellcheck PASS, shfmt PASS. No version discipline detected in-repo
  (release tags drive versions); skipping bump step.
- Wave 2 (G7-G10): delivered as one code commit. 12 files changed; all four
  groups refutation-verified NOT REFUTED (no refutations this wave). Wave
  verification: go build/vet, GOOS=windows build/vet, golangci-lint 0 issues,
  go test -race for telemetry/state/control/secret — green except exactly the
  known-red environmental state/fs subtest. Deferrals recorded below.
- Wave 3 (G11-G14): delivered as one code commit. All four groups
  refutation-verified NOT REFUTED. Wave verification: go build/vet,
  GOOS=windows build/vet, golangci-lint 0 issues, go test -race for
  jobindex/controller/healthwatch all ok. G13's refuter disclosed one
  unreproduced full-suite flake (name lost to output truncation; 42+
  clean runs since, differential baseline clean) — recorded, not
  attributed to the diff.
- Wave 4 (G15-G18): delivered as one code commit. All four groups
  refutation-verified NOT REFUTED (regex language equality probed over 200k
  strings for G18's merge; differential probes for host parse/loop changes).
  Wave verification: go build/vet, GOOS=windows build/vet, golangci-lint
  0 issues, go test -race for host and runtime/docker ok, runtime-docker
  fuzz lane re-run clean.
