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
| G1 | root-config | 4 (9 deferred read-only) | 1 | pending |
| G2 | ci-scripts | 10 | 1 | pending |
| G3 | powershell | 3 | 1 | pending |
| G4 | worker-image | 8 | 1 | pending |
| G5 | go-shared-small | 9 | 1 | pending |
| G6 | go-scaleset | 5 | 1 | pending |
| G7 | go-telemetry | 6 | 2 | pending |
| G8 | go-state | 14 | 2 | pending |
| G9 | go-control | 8 | 2 | pending |
| G10 | go-secret | 15 | 2 | pending |
| G11 | go-jobindex | 8 | 3 | pending |
| G12 | go-controller-src | 9 | 3 | pending |
| G13 | go-controller-tests | 11 | 3 (after G12) | pending |
| G14 | go-healthwatch | 7 | 3 (after G11) | pending |
| G15 | go-host-core | 19 | 4 | pending |
| G16 | go-host-probes | 10 | 4 (after G15) | pending |
| G17 | go-host-tests | 8 | 4 (after G15+G16) | pending |
| G18 | go-runtime-docker | 13 | 4 | pending |
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

(further items populated as waves complete)

## Wave delivery log

(populated as waves complete)
