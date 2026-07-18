# Design resolution — runner-performance

outcome: early-exit
tier: B (light design)
date: 2026-07-17

## Reason

No new types, modules, or package topology. The work is: (1) aligning an existing capacity-gate
basis in `internal/controller/plan.go` with the memory pool workers actually run in, (2) explicit
WSL2 VM sizing via `.wslconfig` (host config), (3) workflow YAML restructuring in medley and
ci-workflows (checkout filter inputs, job consolidation, selector shape) on documented actions
inputs. Contracts touched are existing ones (selector fail-open/fail-closed semantics,
runner-policy allowlist) and are preserved, not redesigned.

## Type sketch

Only candidate type-level change: the memory-availability source consumed by
`affordableWorkerCount` (`internal/controller/plan.go:414-439`). Today it reads host
`AvailablePhysical` (`internal/host/monitor_windows.go:117`). The fix keeps the existing
`host.Monitor` interface shape and changes the reading's basis (WSL2 VM pool vs Windows host),
or the gate's budget input — a localized contract tweak, no new abstractions. Exact shape is a
plan decision (Phase 1 verification decides), not a design-thread.
