# Batch Simplify Results

Scope: repo — whole repository (`/code-tidying:batch-simplify`, repo mode)
Files scanned: 244 in universe; 210 survived Phase 2 filters; 201 swept after
grounding narrowed 9 standards-synced root configs to read-only deferred class
Groups processed: 20, in 5 dependency-ordered waves
Net code diff vs main: 57 files, +331/−576 (−245 lines)
Filed: [#313](https://github.com/melodic-software/ci-runner/issues/313)
(shared atomic-durable-write helper — the sweep's one High-tier concern)

Authorization note (repo-mode requirement): the interactive confirmation gate
was passed on the user's up-front instruction to run the whole-repository sweep
autonomously end to end; the inventory was recorded in RUN-STATE.md rather than
interactively confirmed. Delivery deviated from repo-mode's PR-per-wave default
per session constraints: five wave commits on
`claude/repo-code-tidying-simplify-z75kiv`, no PRs.

| # | Group | Files | Changes | Deferred | Refuter | Verification |
|---|---|---|---|---|---|---|
| G1 | root-config | 4 (+9 read-only) | none | 2 | n/a (no diff) | PASS |
| G2 | ci-scripts | 10 | 3 files | 6 | NOT REFUTED | PASS (97/97 node) |
| G3 | powershell | 3 | 1 file | 9+1 | REFUTED → reverted → safe subset re-applied | PASS |
| G4 | worker-image | 8 | 2 files | 4 | NOT REFUTED | PASS (image build unmapped locally; CI lane covers) |
| G5 | go-shared-small | 9 | 2 files | 4 | NOT REFUTED | PASS |
| G6 | go-scaleset | 5 | 1 file | 5 | NOT REFUTED | PASS |
| G7 | go-telemetry | 6 | 3 files | 3 | NOT REFUTED | PASS |
| G8 | go-state | 14 | 5 files | 4 | NOT REFUTED | PASS (known-red excl.) |
| G9 | go-control | 8 | 1 file | 5 | NOT REFUTED (panic-unwind note) | PASS (+fuzz) |
| G10 | go-secret | 15 | 3 files | 4 | NOT REFUTED | PASS |
| G11 | go-jobindex | 8 | 5 files | 7 | NOT REFUTED | PASS |
| G12 | go-controller-src | 9 | 8 files | 8 | NOT REFUTED | PASS |
| G13 | go-controller-tests | 11 | 2 files | 4 | NOT REFUTED (flake disclosed, unreproduced ×42) | PASS |
| G14 | go-healthwatch | 7 | 4 files | 3 | NOT REFUTED | PASS |
| G15 | go-host-core | 19 | 2 files | 8 | NOT REFUTED | PASS |
| G16 | go-host-probes | 10 | 2 files | 1 | NOT REFUTED | PASS |
| G17 | go-host-tests | 8 | 2 files | 5 | NOT REFUTED | PASS |
| G18 | go-runtime-docker | 13 | 6 files | 5 | NOT REFUTED | PASS (+fuzz) |
| G19 | go-app-src | 23 | 4 files | 11 | NOT REFUTED | PASS (+trimpath builds) |
| G20 | go-app-tests | 11 | 1 file | 3 | NOT REFUTED | PASS |

Final cross-ecosystem union verification: **PASS** —

- `go build ./...`, `go vet ./...`: PASS (linux and GOOS=windows)
- `go mod tidy -diff` gate: PASS
- `golangci-lint run ./...` (v2.6.0 per AGENTS.md): 0 issues
- `go test -race -count=1 ./...`: 13 packages ok; the ONLY failure module-wide
  is the pre-existing environmental subtest
  (`state/fs` open-failure injection vs uid 0), identical to the pre-sweep
  baseline — not a sweep regression
- `node --test .github/scripts/*.test.cjs`: 97/97, exit 0
- `pwsh ./scripts/Test-ReleasePins.ps1`: PASS
- shellcheck 0.11.0 (CI pin) + shfmt: PASS over all shell scripts
- Windows `-trimpath` builds of both commands: PASS
- markdownlint over sweep docs: 0 issues
- UNMAPPED locally (honest gaps, CI-covered): worker image build/verify
  (no Docker daemon), PSScriptAnalyzer (not installed; policy-lane scripts
  verified via Test-ReleasePins + parse checks), Windows-only tests
  (compile-checked via GOOS=windows vet; not executable on Linux)

## Deferred items (filed)

- #313: refactor(persistence): extract a shared atomic-durable-write helper
  (High tier — the temp-write→sync→harden→replace skeleton implemented ~7×)

## Deferred items (not filed — persisted in RUN-STATE.md)

Eight Medium concerns (timer-drain modernization ×10 sites, jobindex loader
dedup, doctor-test harness, monitor close-incident dedup,
Test-DependencyFreshness dedups, WaitGroup.Go/errors.New/errors.AsType
consistency completions) and a Low class, all verbatim in RUN-STATE.md per the
repo-mode High-only filing rule.

## Deferred items (judgment calls preserved, not filed)

The G3 regex swap (refuted — AutomationNull divergence), deliberate defensive
code (http.DefaultClient inheritance, defense-in-depth checks, documenting
copies), and behavior-changing "fixes" out of scope for a preserving sweep
($null flips, NewLazySystemDLL, process.exit swaps). Upstream-owned items
(9 standards-synced configs, cloud-bootstrap.sh) belong in
melodic-software/standards / the plugin distribution repo.

## Scope diagnostic

~75 deferrals vs ~55 applied changes is dominated by deliberately-defensive
code the sweep correctly refused to touch and by one structural concern (#313)
no file-scoped simplifier could safely fix — not evidence of structural rot.
The refutation layer earned its cost: one real behavior divergence caught and
reverted (G3), one honest flake disclosure chased to ground (G13), and one
nil-vs-empty edge proven unobservable (G19).
