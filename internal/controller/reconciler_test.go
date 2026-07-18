package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	clockpkg "github.com/melodic-software/ci-runner/internal/clock"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/scaleset"
	statepkg "github.com/melodic-software/ci-runner/internal/state"
)

func TestReconcilerCreatesOneWarmWorkerAndAdvertisesFullServiceCapacity(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := harness.runtime.startCount(); got != 1 {
		t.Fatalf("start count = %d, want 1", got)
	}
	if result.Observed.Phase != model.PhaseReady {
		t.Fatalf("phase = %s; problems=%#v", result.Observed.Phase, result.Observed.Problems)
	}
	if len(result.Observed.Pools) != 1 || result.Observed.Pools[0].MaxCapacity != 3 || result.Observed.Pools[0].DesiredWorkers != 1 {
		t.Fatalf("pool = %#v", result.Observed.Pools)
	}
	if len(result.Observed.Workers) != 1 || result.Observed.Workers[0].State != model.WorkerIdle {
		t.Fatalf("workers = %#v", result.Observed.Workers)
	}
	for _, call := range harness.scaleSets.SnapshotCalls() {
		if call.Operation == "statistics" && call.MaxCapacity != 3 {
			t.Fatalf("first listener poll maxCapacity = %d, want service capacity 3", call.MaxCapacity)
		}
	}
}

func TestReconcilerReportsPriorCheckpointAgeForFreshnessTelemetry(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	harness.clock.Advance(11 * time.Second)
	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.CheckpointAge != 11*time.Second || !result.CheckpointAgeValid {
		t.Fatalf("checkpoint age = %s, want 11s", result.CheckpointAge)
	}
	if snapshot := telemetrySnapshot(result); snapshot.CheckpointAge != 11*time.Second || !snapshot.CheckpointAgeValid || !snapshot.Valid {
		t.Fatalf("telemetry snapshot = %#v", snapshot)
	}
}

func TestTelemetrySnapshotPreservesPowerGateWhenOperationFailureDegradesObservedPhase(t *testing.T) {
	t.Parallel()
	result := ReconcileResult{
		Observed: model.ObservedState{SchemaVersion: 1, Phase: model.PhaseDegraded},
		Plan:     Plan{Phase: model.PhasePowerSuspended},
	}
	snapshot := telemetrySnapshot(result)
	if !snapshot.PowerGateBlocked {
		t.Fatalf("power gate blocked = false for degraded observed phase with power-suspended plan: %#v", snapshot)
	}
}

func TestReconcilerOmitsCheckpointAgeWithoutValidPriorCheckpoint(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		configure func(*testing.T, *harness)
	}{
		{name: "missing"},
		{name: "future", configure: func(t *testing.T, h *harness) {
			future := h.clock.Now().Add(time.Minute)
			if err := h.store.SaveObserved(context.Background(), model.ObservedState{SchemaVersion: 1, HeartbeatAt: future}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newHarness(t, model.ModeEnabled)
			if test.configure != nil {
				test.configure(t, harness)
			}
			result, err := harness.controller.Step(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.CheckpointAgeValid || telemetrySnapshot(result).CheckpointAgeValid {
				t.Fatalf("invalid prior checkpoint reported as fresh: %#v", result)
			}
		})
	}
}

func TestReconcilerStartsWorkerWithTargetEffectiveLimits(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	cpus := 4.0
	memory := config.ByteSize(24 << 30)
	memorySwap := config.ByteSize(24 << 30)
	pids := int64(8192)
	harness.controller.config.GitHub.Targets[0].Resources.Worker = &config.WorkerOverrides{
		CPUs: &cpus, Memory: &memory, MemorySwap: &memorySwap, PIDs: &pids,
	}

	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := harness.runtime.startRequests()
	if len(requests) != 1 {
		t.Fatalf("start requests = %#v", requests)
	}
	want := config.Worker{CPUs: cpus, Memory: memory, MemorySwap: memorySwap, PIDs: pids}
	if requests[0].Limits != want {
		t.Fatalf("worker limits = %#v, want %#v", requests[0].Limits, want)
	}
}

func TestEnabledStartsDesktopBeforeWorkerInventory(t *testing.T) {
	t.Parallel()
	trace := &callTrace{}
	harness := newHarness(t, model.ModeEnabled)
	harness.desktop.status = model.DesktopStatus{}
	harness.desktop.trace = trace
	harness.runtime.trace = trace

	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := harness.desktop.startCount(); got != 1 {
		t.Fatalf("Desktop starts = %d, want 1", got)
	}
	if got := harness.runtime.startCount(); got != 1 {
		t.Fatalf("worker starts = %d, want 1", got)
	}
	entries := trace.snapshot()
	desktopStart, firstInventory := indexOf(entries, "desktop:start"), indexOf(entries, "workers:list")
	if desktopStart < 0 || firstInventory < 0 || desktopStart > firstInventory {
		t.Fatalf("operation order = %v; Desktop must start before Docker inventory", entries)
	}
	if result.Observed.Phase != model.PhaseReady {
		t.Fatalf("phase = %s; problems=%#v", result.Observed.Phase, result.Observed.Problems)
	}
}

func TestDesktopBootstrapPrecedesResourceAdmissionFailure(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.desktop.status = model.DesktopStatus{}
	resourceErr := errors.New("resource monitor unavailable")
	harness.controller.deps.Resources = staticResources{err: resourceErr}

	result, err := harness.controller.Step(context.Background())
	if !errors.Is(err, resourceErr) {
		t.Fatalf("error = %v, want resource failure", err)
	}
	if got := harness.desktop.startCount(); got != 1 {
		t.Fatalf("Desktop starts = %d, want bootstrap despite resource failure", got)
	}
	if got := harness.runtime.startCount(); got != 0 {
		t.Fatalf("worker starts = %d, want resource admission to fail closed", got)
	}
	if result.Observed.Phase != model.PhaseDegraded || result.Observed.Pools[0].MaxCapacity != 0 {
		t.Fatalf("observed = %#v", result.Observed)
	}
	assertProblemCode(t, result.Observed.Problems, "resource-monitor-error")
}

func TestPersistentPowerMonitorFailureCompletesOneFailClosedStep(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Power.Policy = config.PowerACOnly
	power := &countingPower{err: errors.New("power API unavailable")}
	harness.controller.deps.Power = power
	delayed := &delayedScaleSet{Client: harness.scaleSets, delay: 15 * time.Millisecond}
	harness.controller.deps.ScaleSets = delayed
	if err := harness.controller.setWatchIntervalForTest(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := harness.controller.Step(ctx)
	if err == nil || !strings.Contains(err.Error(), "power API unavailable") {
		t.Fatalf("step error = %v", err)
	}
	if calls := power.callCount(); calls != 1 {
		t.Fatalf("persistent power error caused %d observations in one Step", calls)
	}
	if delayed.callCount() != 1 || result.Observed.Pools[0].MaxCapacity != 0 {
		t.Fatalf("statistics calls=%d observed=%#v", delayed.callCount(), result.Observed)
	}
}

func TestAmbiguousWorkerInventoryFailureFailsClosedWithoutDesktopRestart(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	inventoryErr := errors.New("ambiguous Docker inventory failure")
	harness.runtime.listErr = inventoryErr

	result, err := harness.controller.Step(context.Background())
	if !errors.Is(err, inventoryErr) {
		t.Fatalf("error = %v, want inventory failure", err)
	}
	if got := harness.desktop.startCount(); got != 0 {
		t.Fatalf("Desktop starts = %d, want no restart inferred from inventory failure", got)
	}
	if got := harness.runtime.startCount(); got != 0 {
		t.Fatalf("worker starts = %d, want fail-closed inventory", got)
	}
	if result.Observed.Phase != model.PhaseDegraded || result.Observed.Pools[0].MaxCapacity != 0 {
		t.Fatalf("observed = %#v", result.Observed)
	}
	assertProblemCode(t, result.Observed.Problems, "worker-inventory-error")
}

func TestStoppedDesktopIsHealthyWhenModeDoesNotRequireEngine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		mode  model.Mode
		phase model.Phase
	}{
		{name: "disabled", mode: model.ModeDisabled, phase: model.PhaseDisabled},
		{name: "gaming", mode: model.ModeGaming, phase: model.PhaseGaming},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newHarness(t, test.mode)
			harness.desktop.status = model.DesktopStatus{}

			result, err := harness.controller.Step(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.Observed.Phase != test.phase {
				t.Fatalf("phase = %s, want %s; problems=%#v", result.Observed.Phase, test.phase, result.Observed.Problems)
			}
			if got := harness.desktop.startCount(); got != 0 {
				t.Fatalf("Desktop starts = %d, want 0", got)
			}
			if got := harness.runtime.listCount(); got != 0 {
				t.Fatalf("Docker inventory calls = %d, want 0 for confirmed stopped Desktop", got)
			}
		})
	}
}

func TestDesktopBootstrapRecoversAfterTransientStartFailure(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.desktop.status = model.DesktopStatus{}
	startErr := errors.New("transient Desktop startup failure")
	harness.desktop.setStartError(startErr)

	first, err := harness.controller.Step(context.Background())
	if !errors.Is(err, startErr) {
		t.Fatalf("first error = %v, want startup failure", err)
	}
	if first.Observed.Phase != model.PhaseDegraded || harness.runtime.startCount() != 0 {
		t.Fatalf("first observed = %#v; workers=%#v", first.Observed, harness.runtime.snapshot())
	}
	// A down desktop must not weaponize the resource gate: the host resource
	// observation stays valid through the failed start, so recovery needs no
	// invalid-observation hysteresis once the desktop comes up.
	if first.Observed.ResourceGate.Blocked {
		t.Fatalf("transient desktop start failure blocked the resource gate: %#v", first.Observed.ResourceGate)
	}

	harness.desktop.setStartError(nil)
	second, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Observed.Phase != model.PhaseReady || harness.runtime.startCount() != 1 {
		t.Fatalf("second observed = %#v; workers=%#v", second.Observed, harness.runtime.snapshot())
	}
	// While the desktop is down, both start sites fire in a Step: the eager
	// bootstrap and BuildPlan's StartDesktop fallback (no longer suppressed by a
	// resource gate). The first Step makes two failing attempts, the second one
	// succeeding attempt.
	if got := harness.desktop.startCount(); got != 3 {
		t.Fatalf("Desktop starts = %d, want two failed attempts and one recovery", got)
	}
}

func TestStoppedDesktopPreservesResourceObservation(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.desktop.status = model.DesktopStatus{} // stopped: engine unreachable
	harness.desktop.setStartError(errors.New("desktop unavailable"))

	result, err := harness.controller.Step(context.Background())
	if err == nil {
		t.Fatal("expected the failed desktop start to surface an error")
	}
	// The host resource monitor reads physical RAM independent of Docker, so a
	// stopped desktop must not zero it and trip the gate closed (issue #66) --
	// otherwise the blocked gate would keep the StartDesktop bootstrap from
	// running on re-enable.
	if result.Observed.ResourceGate.Blocked {
		t.Fatalf("stopped desktop blocked the resource gate: %#v", result.Observed.ResourceGate)
	}
	if result.Observed.Resources.TotalMemoryBytes == 0 {
		t.Fatalf("real host resource observation was zeroed while the desktop was stopped: %#v", result.Observed.Resources)
	}
}

func TestReconcilerAdvertisesZeroBeforeDrainRemoval(t *testing.T) {
	t.Parallel()
	trace := &callTrace{}
	harness := newHarness(t, model.ModeDisabled)
	harness.runtime.trace = trace
	harness.runtime.workers = []model.Worker{
		{ID: "busy", PoolID: "org", State: model.WorkerBusy, JobID: "job-1"},
		{ID: "idle", PoolID: "org", State: model.WorkerIdle},
	}
	harness.controller.deps.ScaleSets = &tracingScaleSet{Client: harness.scaleSets, trace: trace}
	first, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed.Phase != model.PhaseDraining || !harness.runtime.hasWorker("idle") {
		t.Fatalf("first quiescence observation = %#v workers=%#v", first.Observed, harness.runtime.snapshot())
	}
	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Observed.Phase != model.PhaseDraining {
		t.Fatalf("phase = %s", result.Observed.Phase)
	}
	entries := trace.snapshot()
	statisticsIndex, deregisterIndex, removeIndex := indexOf(entries, "statistics:0"), indexOf(entries, "deregister:org:1001"), indexOf(entries, "remove:idle")
	if statisticsIndex < 0 || deregisterIndex < 0 || removeIndex < 0 || statisticsIndex > deregisterIndex || deregisterIndex > removeIndex {
		t.Fatalf("operation order = %v", entries)
	}
	if harness.runtime.hasWorker("idle") || !harness.runtime.hasWorker("busy") {
		t.Fatalf("workers = %#v", harness.runtime.snapshot())
	}
}

func TestDrainNeverRetiresOnAssignmentAfterFirstZeroPoll(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	harness.runtime.workers = []model.Worker{{ID: "idle", Name: "runner", PoolID: "org", RunnerID: 44, State: model.WorkerIdle}}
	first, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed.Pools[0].ZeroCapacityConfirmations != 1 {
		t.Fatalf("first observation = %#v", first.Observed.Pools[0])
	}
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{TotalAssignedJobs: 1}
	second, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !harness.runtime.hasWorker("idle") || second.Observed.Pools[0].ZeroCapacityConfirmations != 0 {
		t.Fatalf("assignment race retired worker: %#v", second.Observed)
	}
	for _, call := range harness.scaleSets.SnapshotCalls() {
		if call.Operation == "remove-runner" {
			t.Fatal("assignment race deregistered a potentially active runner")
		}
	}
}

func TestEnabledPoolShrinksThreeIdleWorkersToOneThroughTwoZeroPolls(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.runtime.workers = []model.Worker{
		{ID: "idle-1", Name: "runner-1", PoolID: "org", RunnerID: 41, State: model.WorkerIdle},
		{ID: "idle-2", Name: "runner-2", PoolID: "org", RunnerID: 42, State: model.WorkerIdle},
		{ID: "idle-3", Name: "runner-3", PoolID: "org", RunnerID: 43, State: model.WorkerIdle},
	}
	first, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed.Phase != model.PhaseDraining || first.Observed.Pools[0].MaxCapacity != 0 || first.Observed.Pools[0].ZeroCapacityConfirmations != 1 || len(harness.runtime.snapshot()) != 3 {
		t.Fatalf("first quiesce = %#v workers=%#v", first.Observed, harness.runtime.snapshot())
	}
	second, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(harness.runtime.snapshot()) != 1 || second.Observed.Pools[0].MaxCapacity != 0 || second.Observed.Pools[0].ZeroCapacityConfirmations != 2 {
		t.Fatalf("second quiesce = %#v workers=%#v", second.Observed, harness.runtime.snapshot())
	}
	third, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.Observed.Pools[0].MaxCapacity != 3 || third.Observed.Phase != model.PhaseReady {
		t.Fatalf("restored pool = %#v", third.Observed)
	}
	removed := 0
	for _, call := range harness.scaleSets.SnapshotCalls() {
		if call.Operation == "remove-runner" {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("runner deregistrations = %d, want exactly two", removed)
	}
}

// TestWorkerRetirementCapsDeregistrationsPerStepAndDefersRemainder proves the
// fix for a reviewer-flagged watchdog gap: deregisterRunner runs a full
// GitHub RetryValue budget per call, and internal/app's reconcileStepTimeout
// only budgets Resources.MaximumConcurrentWorkers worth of those per Step.
// Lowering MaximumConcurrentWorkers (simulated here after warm inventory was
// already started under the higher configured limit) can legitimately leave
// more idle workers eligible for retirement than the new cap allows for. The
// removal loop must cap deregisterRunner calls at MaximumConcurrentWorkers
// per Step -- deferring, never dropping, the remainder to a later Step -- so
// the watchdog's budget is never exceeded by a single Step's retirements.
func TestWorkerRetirementCapsDeregistrationsPerStepAndDefersRemainder(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	// Simulate MaximumConcurrentWorkers (or warm capacity) having been lowered
	// after five idle workers were legitimately started under a higher limit:
	// the new cap is well below the number of idle workers now eligible for
	// retirement (target WarmIdle=1 keeps exactly one, so four must drain).
	harness.controller.config.Resources.MaximumConcurrentWorkers = 2
	harness.runtime.workers = []model.Worker{
		{ID: "idle-1", Name: "runner-1", PoolID: "org", RunnerID: 41, State: model.WorkerIdle},
		{ID: "idle-2", Name: "runner-2", PoolID: "org", RunnerID: 42, State: model.WorkerIdle},
		{ID: "idle-3", Name: "runner-3", PoolID: "org", RunnerID: 43, State: model.WorkerIdle},
		{ID: "idle-4", Name: "runner-4", PoolID: "org", RunnerID: 44, State: model.WorkerIdle},
		{ID: "idle-5", Name: "runner-5", PoolID: "org", RunnerID: 45, State: model.WorkerIdle},
	}
	logger := &testLogSink{}
	harness.controller.deps.Logs = logger

	countRemovals := func() int {
		removed := 0
		for _, call := range harness.scaleSets.SnapshotCalls() {
			if call.Operation == "remove-runner" {
				removed++
			}
		}
		return removed
	}
	countDeferrals := func() int {
		logger.mu.Lock()
		defer logger.mu.Unlock()
		deferred := 0
		for _, event := range logger.events {
			if event.Code == "worker-retirement-deferred-step-budget" {
				deferred++
			}
		}
		return deferred
	}

	first, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed.Pools[0].ZeroCapacityConfirmations != 1 || len(harness.runtime.snapshot()) != 5 {
		t.Fatalf("first quiesce poll = %#v workers=%#v", first.Observed.Pools[0], harness.runtime.snapshot())
	}

	// Second step: quiescence reaches its two-poll confirmation threshold and
	// four workers become eligible for retirement, but the deregistration cap
	// (MaximumConcurrentWorkers=2) must limit this Step to exactly two
	// deregisterRunner calls, deferring the other two rather than exceeding
	// the watchdog's per-step retry budget.
	second, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := countRemovals(); got != 2 {
		t.Fatalf("deregistrations after capped step = %d, want exactly 2 (MaximumConcurrentWorkers)", got)
	}
	if got := len(harness.runtime.snapshot()); got != 3 {
		t.Fatalf("workers remaining after capped step = %d, want 3 (1 kept + 2 deferred)", got)
	}
	if got := countDeferrals(); got != 2 {
		t.Fatalf("worker-retirement-deferred-step-budget events = %d, want exactly 2", got)
	}
	if second.Observed.Phase != model.PhaseDraining {
		t.Fatalf("phase after capped step = %s, want still draining", second.Observed.Phase)
	}

	// Remaining steps: the two deferred workers must not be lost. Keep
	// stepping until the drain converges to the single kept warm-idle worker,
	// asserting on every step that the deregistration cap is never exceeded
	// (each step issues at most MaximumConcurrentWorkers=2 new
	// deregistrations) and that the cumulative total lands on exactly 4 -- the
	// full set of originally-excess workers, neither double-counted nor
	// stranded. This intentionally avoids asserting the exact step at which
	// Phase reports Ready and MaxCapacity is fully restored (that timing
	// depends on the capacity-acknowledgment sequencing exercised by
	// TestEnabledPoolShrinksThreeIdleWorkersToOneThroughTwoZeroPolls, which is
	// orthogonal to this fix) and instead focuses precisely on what the fix
	// guarantees: the per-step cap holds, and deferred work is never dropped.
	const maxAdditionalSteps = 6
	previousRemovals := countRemovals()
	converged := false
	for i := 0; i < maxAdditionalSteps; i++ {
		if _, err := harness.controller.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		removals := countRemovals()
		if delta := removals - previousRemovals; delta > 2 {
			t.Fatalf("additional step %d issued %d new deregistrations, want at most 2 (MaximumConcurrentWorkers)", i, delta)
		}
		previousRemovals = removals
		if len(harness.runtime.snapshot()) == 1 {
			converged = true
			break
		}
	}
	if !converged {
		t.Fatalf("workers did not converge to the single kept worker within %d additional steps; remaining=%#v", maxAdditionalSteps, harness.runtime.snapshot())
	}
	if got := countRemovals(); got != 4 {
		t.Fatalf("cumulative deregistrations after convergence = %d, want exactly 4 (all originally-excess workers eventually drained, none dropped)", got)
	}
}

// TestWorkerRetirementRotatesStartingWorkerToAvoidHeadOfLineBlocker proves the
// fix for a reviewer-flagged starvation gap in the per-step retirement cap
// (TestWorkerRetirementCapsDeregistrationsPerStepAndDefersRemainder above):
// with MaximumConcurrentWorkers=1, plan.Remove's first entry sorts
// identically every Step. If that first worker's deregisterRunner call keeps
// returning a persistent error, a fixed head-of-line iteration order would
// let it consume the single per-step retry slot every Step forever --
// before the cap was added, the loop simply recorded the error and moved on
// to other idle workers -- starving every worker behind it in plan.Remove
// from ever being retired. The removal loop must rotate which worker is
// tried first each Step (retirementCursor, mirroring
// TestRegistrationCheckCapsCallsPerStepAndRotatesCandidates's
// registrationCheckCursor below) so later plan.Remove entries eventually get
// a turn even while the first worker keeps failing.
func TestWorkerRetirementRotatesStartingWorkerToAvoidHeadOfLineBlocker(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Resources.MaximumConcurrentWorkers = 1
	harness.runtime.workers = []model.Worker{
		{ID: "idle-1", Name: "runner-1", PoolID: "org", RunnerID: 41, State: model.WorkerIdle},
		{ID: "idle-2", Name: "runner-2", PoolID: "org", RunnerID: 42, State: model.WorkerIdle},
		{ID: "idle-3", Name: "runner-3", PoolID: "org", RunnerID: 43, State: model.WorkerIdle},
	}
	// idle-1's deregistration always fails, simulating a persistent GitHub
	// error for that one runner; every other runner's deregistration succeeds
	// through the same fake.
	persistentErr := errors.New("persistent deregistration failure")
	harness.controller.deps.ScaleSets = &runnerRemovalFailureClient{Client: harness.scaleSets, failRunnerID: 41, err: persistentErr}

	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	} // first zero-capacity poll; not yet eligible for retirement

	const maxSteps = 8
	idle2Removed := false
	for step := 0; step < maxSteps; step++ {
		// A Step that attempts idle-1's deregistration surfaces the injected
		// persistentErr through record()'s operationErrors (Step() joins and
		// returns them), exactly like a real retryable GitHub error would. That
		// is the scenario under test, not a test failure: only an error other
		// than the expected persistent one is unexpected here.
		if _, err := harness.controller.Step(context.Background()); err != nil && !errors.Is(err, persistentErr) {
			t.Fatal(err)
		}
		if !harness.runtime.hasWorker("idle-2") {
			idle2Removed = true
			break
		}
	}
	if !idle2Removed {
		t.Fatalf("idle-2 was never retired within %d steps; a persistently failing head-of-line worker starved every worker behind it in plan.Remove", maxSteps)
	}
	if !harness.runtime.hasWorker("idle-1") {
		t.Fatal("idle-1 was removed despite its deregisterRunner call always failing")
	}
	if !harness.runtime.hasWorker("idle-3") {
		t.Fatal("idle-3 (the single retained warm-idle worker) was unexpectedly removed")
	}
}

// TestReconcilerEffectiveMaximumConcurrentWorkersReflectsDesiredOverride
// proves the fix for a reviewer-flagged watchdog gap: internal/app's
// reconcile-step watchdog queries Reconciler.EffectiveMaximumConcurrentWorkers
// before every Step to size the JIT-start portion of its budget from the same
// effective limit BuildPlan actually applies (static cap or, when set, the
// durable Desired.TemporaryCapacityOverride), not the static cap alone. This
// exercises that resolution against the real durable state store: no
// override yet returns the static cap, an override in effect returns the
// override (even when larger), and a desired-state read failure fails safe
// to the static cap rather than assuming an unverifiable override.
func TestReconcilerEffectiveMaximumConcurrentWorkersReflectsDesiredOverride(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Resources.MaximumConcurrentWorkers = 1

	if got := harness.controller.EffectiveMaximumConcurrentWorkers(context.Background()); got != 1 {
		t.Fatalf("no override: EffectiveMaximumConcurrentWorkers = %d, want static cap 1", got)
	}

	override := 10
	if err := harness.store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, TemporaryCapacityOverride: &override, UpdatedAt: harness.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := harness.controller.EffectiveMaximumConcurrentWorkers(context.Background()); got != 10 {
		t.Fatalf("override=10 > static cap=1: EffectiveMaximumConcurrentWorkers = %d, want the override 10", got)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := harness.controller.EffectiveMaximumConcurrentWorkers(cancelled); got != 1 {
		t.Fatalf("desired-state read failure: EffectiveMaximumConcurrentWorkers = %d, want fail-safe static cap 1 (not the unverifiable override)", got)
	}
}

// TestRegistrationCheckCapsCallsPerStepAndRotatesCandidates proves the fix
// for a third reviewer-flagged watchdog gap: RunnerRegistered runs a full
// GitHub RetryValue budget per call, and the idle-worker inventory eligible
// for this JIT-cancellation check is not itself bounded by
// Resources.MaximumConcurrentWorkers. step()'s registration-check loop must
// cap RunnerRegistered calls at Resources.MaximumConcurrentWorkers per Step
// and rotate which candidates get picked via registrationCheckCursor, so a
// fixed from-the-front cap (which would always re-check the same leading
// candidates while starving the rest forever) cannot happen. This asserts
// the cap holds every step and, allowing for the same excess-idle-worker
// retirement this population would also legitimately trigger, that every
// worker is eventually accounted for by either a registration check or a
// retirement -- never silently skipped forever by both.
func TestRegistrationCheckCapsCallsPerStepAndRotatesCandidates(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Resources.MaximumConcurrentWorkers = 2
	harness.runtime.workers = []model.Worker{
		{ID: "idle-1", Name: "runner-1", PoolID: "org", RunnerID: 41, State: model.WorkerIdle},
		{ID: "idle-2", Name: "runner-2", PoolID: "org", RunnerID: 42, State: model.WorkerIdle},
		{ID: "idle-3", Name: "runner-3", PoolID: "org", RunnerID: 43, State: model.WorkerIdle},
		{ID: "idle-4", Name: "runner-4", PoolID: "org", RunnerID: 44, State: model.WorkerIdle},
		{ID: "idle-5", Name: "runner-5", PoolID: "org", RunnerID: 45, State: model.WorkerIdle},
	}
	const wantWorkers = 5

	checksIssued := func() int {
		checks := 0
		for _, call := range harness.scaleSets.SnapshotCalls() {
			if call.Operation == "runner-registration" {
				checks++
			}
		}
		return checks
	}
	accountedFor := func() map[int64]bool {
		accounted := map[int64]bool{}
		for _, call := range harness.scaleSets.SnapshotCalls() {
			if call.Operation == "runner-registration" || call.Operation == "remove-runner" {
				accounted[call.ScaleSetID] = true
			}
		}
		return accounted
	}

	const maxSteps = 8
	previousChecks := 0
	for step := 0; step < maxSteps; step++ {
		if _, err := harness.controller.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		checks := checksIssued()
		if delta := checks - previousChecks; delta > 2 {
			t.Fatalf("step %d issued %d new registration checks, want at most 2 (MaximumConcurrentWorkers)", step, delta)
		}
		previousChecks = checks
		if len(accountedFor()) == wantWorkers {
			return
		}
	}
	t.Fatalf("registration checks + retirements covered %d/%d workers within %d steps (cap=2 per step); rotation must not starve any candidate: %#v", len(accountedFor()), wantWorkers, maxSteps, accountedFor())
}

func TestEnabledQuiescePreservesAllWorkersWhenAssignmentArrives(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.runtime.workers = []model.Worker{
		{ID: "idle-1", Name: "runner-1", PoolID: "org", RunnerID: 41, State: model.WorkerIdle},
		{ID: "idle-2", Name: "runner-2", PoolID: "org", RunnerID: 42, State: model.WorkerIdle},
		{ID: "idle-3", Name: "runner-3", PoolID: "org", RunnerID: 43, State: model.WorkerIdle},
	}
	first, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed.Pools[0].ZeroCapacityConfirmations != 1 {
		t.Fatalf("first pool = %#v", first.Observed.Pools[0])
	}
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{TotalAssignedJobs: 1}
	second, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(harness.runtime.snapshot()) != 3 || second.Observed.Pools[0].ZeroCapacityConfirmations != 0 {
		t.Fatalf("assignment during quiesce removed capacity: %#v", second.Observed)
	}
	for _, call := range harness.scaleSets.SnapshotCalls() {
		if call.Operation == "remove-runner" {
			t.Fatal("assignment during quiesce deregistered a worker")
		}
	}
}

func TestEnabledQuiesceResumesAfterRestartAndSecondZeroPoll(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.runtime.workers = []model.Worker{
		{ID: "idle-1", Name: "runner-1", PoolID: "org", RunnerID: 41, State: model.WorkerIdle},
		{ID: "idle-2", Name: "runner-2", PoolID: "org", RunnerID: 42, State: model.WorkerIdle},
		{ID: "idle-3", Name: "runner-3", PoolID: "org", RunnerID: 43, State: model.WorkerIdle},
	}
	first, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed.Pools[0].ZeroCapacityConfirmations != 1 {
		t.Fatalf("first pool = %#v", first.Observed.Pools[0])
	}
	restarted, err := NewReconciler(harness.controller.config, "test-version", harness.controller.deps)
	if err != nil {
		t.Fatal(err)
	}
	harness.controller = restarted
	second, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(harness.runtime.snapshot()) != 1 || second.Observed.Pools[0].ZeroCapacityConfirmations != 2 {
		t.Fatalf("restart lost quiesce progress: %#v workers=%#v", second.Observed, harness.runtime.snapshot())
	}
}

func TestSingleExcessIdleRunnerRemainsAvailableWithoutPoolQuiesce(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.runtime.workers = []model.Worker{
		{ID: "warm", Name: "runner-warm", PoolID: "org", RunnerID: 41, State: model.WorkerIdle},
		{ID: "canceled", Name: "runner-canceled", PoolID: "org", RunnerID: 42, State: model.WorkerIdle},
	}
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{TotalAssignedJobs: 1}
	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{}
	for range 3 {
		result, err := harness.controller.Step(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Observed.Phase != model.PhaseReady || result.Observed.Pools[0].MaxCapacity != 3 {
			t.Fatalf("single excess worker interrupted pool service: %#v", result.Observed)
		}
	}
	if len(harness.runtime.snapshot()) != 2 {
		t.Fatalf("single excess worker was retired instead of left for natural convergence: %#v", harness.runtime.snapshot())
	}
	for _, call := range harness.scaleSets.SnapshotCalls() {
		if call.Operation == "remove-runner" {
			t.Fatal("single excess runner was deregistered while the pool remained available")
		}
	}
}

func TestMissingJITRegistrationReplacesOrphanedWarmContainerInSameStep(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.runtime.workers = []model.Worker{{
		ID: "orphan", Name: "runner-orphan", PoolID: "org", RunnerID: 42, State: model.WorkerIdle,
	}}
	harness.scaleSets.MissingRunners[42] = true

	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workers := harness.runtime.snapshot()
	if len(workers) != 1 || workers[0].ID == "orphan" || harness.runtime.startCount() != 1 {
		t.Fatalf("orphan was not replaced: observed=%#v workers=%#v starts=%d", result.Observed, workers, harness.runtime.startCount())
	}
	if result.Observed.Phase != model.PhaseReady || result.Observed.Pools[0].DesiredWorkers != 1 {
		t.Fatalf("replacement did not restore ready capacity: %#v", result.Observed)
	}
	for _, call := range harness.scaleSets.SnapshotCalls() {
		if call.Operation == "remove-runner" && call.ScaleSetID == 42 {
			t.Fatal("already-missing GitHub runner was deregistered instead of retiring only its idle container")
		}
	}
}

func TestMissingJITRegistrationFinalBusyRacePreservesWorker(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.runtime.workers = []model.Worker{{
		ID: "racing", Name: "runner-racing", PoolID: "org", RunnerID: 42, State: model.WorkerIdle,
	}}
	harness.runtime.acquireOnRemove = "racing"
	harness.scaleSets.MissingRunners[42] = true

	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workers := harness.runtime.snapshot()
	if len(workers) != 1 || workers[0].ID != "racing" || workers[0].State != model.WorkerBusy || harness.runtime.startCount() != 0 {
		t.Fatalf("final busy race was not preserved: observed=%#v workers=%#v", result.Observed, workers)
	}
	assertProblemCode(t, result.Observed.Problems, "unregistered-worker-became-busy")
}

func TestRunnerRegistrationLookupFailurePreservesIdleCapacity(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.runtime.workers = []model.Worker{{
		ID: "idle", Name: "runner-idle", PoolID: "org", RunnerID: 42, State: model.WorkerIdle,
	}}
	lookupErr := &scaleset.Error{Kind: scaleset.ErrorForbidden, Operation: "get runner", StatusCode: 403}
	harness.scaleSets.RunnerErrors[42] = lookupErr

	result, err := harness.controller.Step(context.Background())
	if !errors.Is(err, lookupErr) {
		t.Fatalf("error = %v, want registration lookup failure", err)
	}
	if !harness.runtime.hasWorker("idle") || harness.runtime.startCount() != 0 {
		t.Fatalf("uncertain registration mutated capacity: %#v", harness.runtime.snapshot())
	}
	assertProblemCode(t, result.Observed.Problems, "runner-registration-check-error")
}

func TestPostCapacityInventoryFailureSkipsMissingRegistrationCleanup(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.runtime.workers = []model.Worker{{
		ID: "idle", Name: "runner-idle", PoolID: "org", RunnerID: 42, State: model.WorkerIdle,
	}}
	harness.scaleSets.MissingRunners[42] = true
	refreshErr := errors.New("post-capacity Docker inventory failed")
	harness.runtime.listErr = refreshErr
	harness.runtime.listErrAt = 2

	result, err := harness.controller.Step(context.Background())
	if !errors.Is(err, refreshErr) {
		t.Fatalf("error = %v, want post-capacity inventory failure", err)
	}
	if !harness.runtime.hasWorker("idle") || harness.runtime.startCount() != 0 {
		t.Fatalf("stale inventory mutated worker capacity: %#v", harness.runtime.snapshot())
	}
	for _, call := range harness.scaleSets.SnapshotCalls() {
		if call.Operation == "runner-registration" && call.ScaleSetID == 42 {
			t.Fatal("registration was checked from stale pre-capacity worker state")
		}
	}
	assertProblemCode(t, result.Observed.Problems, "worker-refresh-error")
}

func TestUnregisteredWorkerPersistsWhenRemovalAndFinalInventoryFail(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	harness.runtime.workers = []model.Worker{{
		ID: "orphan", Name: "runner-orphan", PoolID: "org", RunnerID: 42, State: model.WorkerIdle,
	}}
	harness.scaleSets.MissingRunners[42] = true
	removeErr := errors.New("graceful container removal failed")
	finalInventoryErr := errors.New("final Docker inventory failed")
	harness.runtime.removeErr = removeErr
	harness.runtime.listErr = finalInventoryErr
	harness.runtime.listErrAt = 3

	result, err := harness.controller.Step(context.Background())
	if !errors.Is(err, removeErr) || !errors.Is(err, finalInventoryErr) {
		t.Fatalf("error = %v, want removal and final inventory failures", err)
	}
	if len(result.Observed.Workers) != 1 || result.Observed.Workers[0].State != model.WorkerUnregistered {
		t.Fatalf("transient unregistered evidence was lost: %#v", result.Observed.Workers)
	}
	persisted, loadErr := harness.store.LoadObserved(context.Background())
	if loadErr != nil || len(persisted.Workers) != 1 || persisted.Workers[0].State != model.WorkerUnregistered {
		t.Fatalf("persisted workers = %#v, error = %v", persisted.Workers, loadErr)
	}
	assertProblemCode(t, result.Observed.Problems, "unregistered-worker-remove-error")
	assertProblemCode(t, result.Observed.Problems, "worker-final-inventory-error")
}

func TestDurableActiveJobOverridesJobWritableIdleState(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	harness.runtime.workers = []model.Worker{{ID: "idle", Name: "runner", PoolID: "org", RunnerID: 44, State: model.WorkerIdle}}
	harness.jobs.active["org\x00runner"] = "job-1"
	harness.scaleSets.MissingRunners[44] = true
	for range 2 {
		if _, err := harness.controller.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !harness.runtime.hasWorker("idle") {
		t.Fatal("job-writable idle state overrode durable active-job evidence")
	}
	for _, call := range harness.scaleSets.SnapshotCalls() {
		if call.Operation == "runner-registration" && call.ScaleSetID == 44 {
			t.Fatal("active-job evidence was ignored in favor of a registration lookup")
		}
	}
}

func TestDrainRetainsIdleWorkerForAssignedJobUntilHookReportsBusy(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	harness.runtime.workers = []model.Worker{{ID: "assigned", Name: "assigned", PoolID: "org", State: model.WorkerIdle}}
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{TotalAssignedJobs: 1}

	first, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !harness.runtime.hasWorker("assigned") || first.Observed.Pools[0].MaxCapacity != 0 || first.Observed.Pools[0].TotalAssignedJobs != 1 {
		t.Fatalf("first drain removed an assigned-but-not-yet-busy worker: %#v", first.Observed)
	}

	harness.runtime.mu.Lock()
	harness.runtime.workers[0].State = model.WorkerBusy
	harness.runtime.workers[0].JobID = "job-1"
	harness.runtime.mu.Unlock()
	second, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !harness.runtime.hasWorker("assigned") || len(second.Observed.Workers) != 1 || second.Observed.Workers[0].State != model.WorkerBusy {
		t.Fatalf("assigned worker was not preserved after becoming busy: %#v", second.Observed.Workers)
	}
}

func TestDrainWarningNeverTerminatesBusyWork(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	harness.runtime.workers = []model.Worker{{ID: "busy", PoolID: "org", State: model.WorkerBusy, JobID: "job-1"}}
	started := harness.clock.Now().Add(-21 * time.Minute)
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseDraining, DrainStartedAt: &started,
		Pools: []model.PoolObservation{{ID: "org", ScaleSetID: 1, ListenerID: "listener-org"}},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertProblemCode(t, result.Observed.Problems, "drain-warning")
	if !harness.runtime.hasWorker("busy") {
		t.Fatal("warning threshold terminated busy work")
	}
}

func TestGamingRacePreservesNewlyBusyWorkerAndDefersShutdown(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeGaming)
	harness.runtime.workers = []model.Worker{{ID: "racing", PoolID: "org", State: model.WorkerIdle}}
	harness.runtime.acquireOnRemove = "racing"
	harness.desktop.status.RunningWSLCount = 1
	first, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !harness.runtime.hasWorker("racing") || first.Observed.Pools[0].ZeroCapacityConfirmations != 1 {
		t.Fatalf("first quiescence observation removed worker: %#v", first.Observed)
	}
	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if harness.desktop.stopCount() != 0 || harness.desktop.wslCount() != 0 {
		t.Fatal("gaming shutdown proceeded after worker acquired work")
	}
	worker := harness.runtime.snapshot()[0]
	if worker.State != model.WorkerBusy {
		t.Fatalf("worker state = %s", worker.State)
	}
	if result.Observed.Phase != model.PhaseDegraded {
		t.Fatalf("phase = %s", result.Observed.Phase)
	}
	assertProblemCode(t, result.Observed.Problems, "worker-became-busy")
	assertProblemCode(t, result.Observed.Problems, "gaming-active-race")
}

func TestReconcilerRestartUsesInventoryWithoutDuplicateWorker(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := NewReconciler(harness.controller.config, "test-version", harness.controller.deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := harness.runtime.startCount(); got != 1 {
		t.Fatalf("restart created duplicate worker; starts = %d", got)
	}
	if got := len(harness.runtime.snapshot()); got != 1 {
		t.Fatalf("worker count = %d", got)
	}
}

func TestConcurrentStepsAreSerializedAndIdempotent(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := harness.controller.Step(context.Background())
			errorsSeen <- err
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := harness.runtime.startCount(); got != 1 {
		t.Fatalf("concurrent reconciliations started %d workers", got)
	}
}

func TestControlStatusAndCommittedShutdownInterruptLongPoll(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	handler, err := NewControlHandler(harness.controller, 1234)
	if err != nil {
		t.Fatal(err)
	}
	stepDone := make(chan error, 1)
	go func() {
		_, stepErr := harness.controller.Step(context.Background())
		stepDone <- stepErr
	}()
	waitForSignal(t, blocking.entered, "listener poll did not begin")

	statusDone := make(chan control.Response, 1)
	go func() {
		statusDone <- handler.Handle(context.Background(), control.Request{
			SchemaVersion: control.SchemaVersion, RequestID: "status-while-polling", Operation: control.OperationStatus,
		})
	}()
	select {
	case response := <-statusDone:
		if !response.OK {
			t.Fatalf("status response = %#v", response)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("status was blocked by the listener poll")
	}

	request := control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "shutdown-while-polling", Operation: control.OperationShutdown,
		Shutdown: &control.ShutdownRequest{
			Reason: "test", ExpectedProcessID: 1234, ExpectedVersion: "test-version", ExpectedActiveJobCount: 0,
		},
	}
	if response := handler.Handle(context.Background(), request); !response.OK {
		t.Fatalf("shutdown response = %#v", response)
	}
	commitDone := make(chan struct{})
	go func() {
		handler.CommitShutdown(request.RequestID)
		close(commitDone)
	}()
	waitForSignal(t, commitDone, "accepted shutdown was blocked by the listener poll")
	select {
	case <-stepDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("accepted shutdown did not cancel the listener poll promptly")
	}
}

func TestDesiredChangeCancelsLongPollAndImmediatelyAdvertisesZero(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	if err := harness.controller.setWatchIntervalForTest(5 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	done := make(chan ReconcileResult, 1)
	go func() {
		result, _ := harness.controller.Step(context.Background())
		done <- result
	}()
	waitForSignal(t, blocking.entered, "listener poll did not begin")
	if err := harness.store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled, UpdatedAt: harness.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if len(result.Observed.Pools) != 1 || !result.Observed.Pools[0].CapacityAcknowledged || result.Observed.Pools[0].MaxCapacity != 0 {
			t.Fatalf("observed pools = %#v", result.Observed.Pools)
		}
	case <-time.After(time.Second):
		t.Fatal("desired-state change did not interrupt and replace the long poll")
	}
	if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[3 0]" {
		t.Fatalf("advertised capacities = %v, want [3 0]", got)
	}
}

func TestDesiredChangeCancelsEnsureRetryBeforeAnyCapacityPoll(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	blocking := newFirstBlockingEnsure(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	if err := harness.controller.setWatchIntervalForTest(5 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	done := make(chan ReconcileResult, 1)
	go func() {
		result, _ := harness.controller.Step(context.Background())
		done <- result
	}()
	waitForSignal(t, blocking.entered, "scale-set ensure did not begin")
	if err := harness.store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled, UpdatedAt: harness.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if len(result.Observed.Pools) != 1 || result.Observed.Pools[0].MaxCapacity != 0 || !result.Observed.Pools[0].CapacityAcknowledged {
			t.Fatalf("replacement step did not advertise zero: %#v", result.Observed.Pools)
		}
	case <-time.After(time.Second):
		t.Fatal("desired change did not cancel Ensure promptly")
	}
}

func TestDesiredChangeCancelsDockerDesktopStart(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	desktop := newBlockingDesktop()
	harness.controller.deps.Desktop = desktop
	if err := harness.controller.setWatchIntervalForTest(5 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	done := make(chan ReconcileResult, 1)
	go func() {
		result, _ := harness.controller.Step(context.Background())
		done <- result
	}()
	waitForSignal(t, desktop.entered, "Docker Desktop start did not begin")
	if err := harness.store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled, UpdatedAt: harness.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if len(result.Observed.Pools) != 1 || result.Observed.Pools[0].MaxCapacity != 0 || !result.Observed.Pools[0].CapacityAcknowledged {
			t.Fatalf("replacement step did not advertise zero: %#v", result.Observed.Pools)
		}
	case <-time.After(time.Second):
		t.Fatal("desired change did not cancel Docker Desktop start promptly")
	}
}

func TestPowerChangeCancelsLongPollAndImmediatelyAdvertisesZero(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Power.Policy = config.PowerACOnly
	acSince := harness.clock.Now().Add(-time.Minute)
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{SchemaVersion: 1, PowerGate: model.PowerGateState{ACSince: &acSince}}); err != nil {
		t.Fatal(err)
	}
	power := &mutablePower{snapshot: model.PowerSnapshot{ACConnected: true, ObservedAt: harness.clock.Now()}}
	harness.controller.deps.Power = power
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	if err := harness.controller.setWatchIntervalForTest(5 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	done := make(chan ReconcileResult, 1)
	go func() {
		result, _ := harness.controller.Step(context.Background())
		done <- result
	}()
	waitForSignal(t, blocking.entered, "listener poll did not begin")
	power.set(false)
	select {
	case result := <-done:
		if len(result.Observed.Pools) != 1 || !result.Observed.Pools[0].CapacityAcknowledged || result.Observed.Pools[0].MaxCapacity != 0 {
			t.Fatalf("observed pools = %#v", result.Observed.Pools)
		}
	case <-time.After(time.Second):
		t.Fatal("power change did not interrupt and replace the long poll")
	}
	if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[3 0]" {
		t.Fatalf("advertised capacities = %v, want [3 0]", got)
	}
}

func TestFreshAdmissionAfterJITPreventsStartAfterDesiredChange(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	flipping := &desiredFlippingScaleSet{Client: harness.scaleSets, store: harness.store, now: harness.clock.Now()}
	harness.controller.deps.ScaleSets = flipping
	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if harness.runtime.startCount() != 0 {
		t.Fatal("worker started from a stale pre-JIT desired-state snapshot")
	}
	if len(result.Observed.Pools) != 1 || !result.Observed.Pools[0].CapacityAcknowledged || result.Observed.Pools[0].MaxCapacity != 0 {
		t.Fatalf("observed pools = %#v", result.Observed.Pools)
	}
}

func TestDesiredFlipAfterAcquireServicesAssignedJobWhileCapacityStaysZero(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{TotalAssignedJobs: 1}
	flipping := &desiredFlippingScaleSet{Client: harness.scaleSets, store: harness.store, now: harness.clock.Now()}
	harness.controller.deps.ScaleSets = flipping

	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if harness.runtime.startCount() != 1 || len(result.Observed.Workers) != 1 {
		t.Fatalf("acquired job was stranded after desired flip: starts=%d observed=%#v", harness.runtime.startCount(), result.Observed)
	}
	if !result.Observed.Pools[0].CapacityAcknowledged || result.Observed.Pools[0].MaxCapacity != 0 || result.Observed.Pools[0].DrainServiceCapacity != 3 {
		t.Fatalf("drain reopened capacity or lost service obligation: %#v", result.Observed.Pools[0])
	}
}

func TestResourceDerivedPlanAdmitsOnlySafeMultiWorkerSlots(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.GitHub.Targets[0].WarmIdle = 3
	harness.controller.deps.Resources = staticResources{snapshot: model.ResourceSnapshot{
		TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 24 << 30, CPUUtilizationPercent: 10,
	}}
	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if harness.runtime.startCount() != 1 || result.Observed.Pools[0].MaxCapacity != 1 {
		t.Fatalf("starts=%d pools=%#v, want one 8-GiB slot above the 25%% floor", harness.runtime.startCount(), result.Observed.Pools)
	}
}

func TestLowMemoryStepRetainsSatisfiedIdleWarmCapacity(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.runtime.workers = []model.Worker{{ID: "idle", Name: "idle", PoolID: "org", RunnerID: 41, State: model.WorkerIdle}}
	harness.controller.deps.Resources = staticResources{snapshot: model.ResourceSnapshot{
		TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 20 << 30, CPUUtilizationPercent: 10,
	}}

	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !harness.runtime.hasWorker("idle") || harness.runtime.startCount() != 0 {
		t.Fatalf("low-memory step changed satisfied worker inventory: %#v", harness.runtime.snapshot())
	}
	if result.Observed.Phase != model.PhaseReady || result.Observed.ResourceGate != (model.ResourceGateState{}) {
		t.Fatalf("low-memory observed gate = %#v phase=%s", result.Observed.ResourceGate, result.Observed.Phase)
	}
	if len(result.Observed.Pools) != 1 || result.Observed.Pools[0].MaxCapacity != 1 || result.Observed.Pools[0].DesiredWorkers != 1 {
		t.Fatalf("low-memory pool observation = %#v", result.Observed.Pools)
	}
}

func TestFreshAdmissionReservesExactMixedProfileMemory(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Resources.MaximumConcurrentWorkers = 2
	highMemory := config.ByteSize(24 << 30)
	codeql := harness.controller.config.GitHub.Targets[0]
	codeql.ID = "codeql"
	codeql.RunnerGroup = "ci-local-codeql"
	codeql.ScaleSetName = "codeql-ubuntu-24.04-x64"
	codeql.Priority = 10
	codeql.Resources.Worker = &config.WorkerOverrides{Memory: &highMemory, MemorySwap: &highMemory}
	harness.controller.config.GitHub.Targets = append(harness.controller.config.GitHub.Targets, codeql)
	harness.controller.deps.Resources = staticResources{snapshot: model.ResourceSnapshot{
		TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 48 << 30, CPUUtilizationPercent: 10,
	}}

	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := harness.runtime.startRequests()
	if len(requests) != 2 || requests[0].PoolID != "org" || requests[1].PoolID != "codeql" {
		t.Fatalf("start requests = %#v", requests)
	}
	if requests[0].Limits.Memory != config.ByteSize(8<<30) || requests[1].Limits.Memory != highMemory {
		t.Fatalf("mixed start limits = %#v", requests)
	}
}

func TestFreshAdmissionSkipsNewlyOversizedStartAndContinuesSmallerPool(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Resources.MaximumConcurrentWorkers = 3
	harness.controller.config.GitHub.Targets[0].Priority = 10
	highMemory := config.ByteSize(24 << 30)
	codeql := harness.controller.config.GitHub.Targets[0]
	codeql.ID = "codeql"
	codeql.RunnerGroup = "ci-local-codeql"
	codeql.ScaleSetName = "codeql-ubuntu-24.04-x64"
	codeql.WarmIdle = 2
	codeql.MaxCapacity = 2
	codeql.Priority = 0
	codeql.Resources.Worker = &config.WorkerOverrides{Memory: &highMemory, MemorySwap: &highMemory}
	harness.controller.config.GitHub.Targets = append(harness.controller.config.GitHub.Targets, codeql)
	initial := model.ResourceSnapshot{TotalMemoryBytes: 128 << 30, AvailableMemoryBytes: 88 << 30, CPUUtilizationPercent: 10}
	contracted := initial
	contracted.AvailableMemoryBytes = 72 << 30
	harness.controller.deps.Resources = &sequenceResources{snapshots: []model.ResourceSnapshot{
		initial, initial, initial, contracted,
	}}

	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := harness.runtime.startRequests()
	if len(requests) != 2 || requests[0].PoolID != "codeql" || requests[1].PoolID != "org" {
		t.Fatalf("start requests = %#v, want one CodeQL start followed by the smaller ordinary start", requests)
	}
}

func TestMemoryReservationSaturatesAndFailsClosed(t *testing.T) {
	t.Parallel()
	maximum := ^uint64(0)
	reserved := saturatingAddUint64(maximum-4, 8)
	if reserved != maximum {
		t.Fatalf("saturated reservation = %d, want %d", reserved, maximum)
	}
	if available := availableAfterMemoryReservation(maximum-1, reserved); available != 0 {
		t.Fatalf("available memory after saturated stale reservation = %d, want fail-closed zero", available)
	}
}

func TestPollFailurePreservesIdleWorkerUntilCapacityAcknowledged(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	harness.runtime.workers = []model.Worker{{ID: "idle", PoolID: "org", State: model.WorkerIdle}}
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Pools: []model.PoolObservation{{
			ID: "org", ScaleSetID: 1, ListenerID: "listener-org", MaxCapacity: 1, CapacityAcknowledged: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	pollErr := &scaleset.Error{Kind: scaleset.ErrorServer, Operation: "poll", StatusCode: 503}
	harness.scaleSets.Errors["statistics:1"] = pollErr
	if err := harness.controller.SetBackoffForTest(BackoffPolicy{Initial: time.Millisecond, Maximum: time.Millisecond, Multiplier: 1, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	result, err := harness.controller.Step(context.Background())
	if !errors.Is(err, pollErr) {
		t.Fatalf("error = %v, want poll failure", err)
	}
	if !harness.runtime.hasWorker("idle") {
		t.Fatal("idle worker was removed without a current capacity acknowledgement")
	}
	if result.Observed.Pools[0].CapacityAcknowledged || result.Observed.Pools[0].MaxCapacity != 1 {
		t.Fatalf("poll failure was persisted as an acknowledgement: %#v", result.Observed.Pools[0])
	}
}

func TestPollFailurePreservesDockerAndWSLDuringGamingDrain(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeGaming)
	harness.desktop.status.RunningWSLCount = 1
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Pools: []model.PoolObservation{{
			ID: "org", ScaleSetID: 1, ListenerID: "listener-org", MaxCapacity: 1, CapacityAcknowledged: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	pollErr := &scaleset.Error{Kind: scaleset.ErrorServer, Operation: "poll", StatusCode: 503}
	harness.scaleSets.Errors["statistics:1"] = pollErr
	if err := harness.controller.SetBackoffForTest(BackoffPolicy{Initial: time.Millisecond, Maximum: time.Millisecond, Multiplier: 1, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.controller.Step(context.Background()); !errors.Is(err, pollErr) {
		t.Fatalf("error = %v, want poll failure", err)
	}
	if harness.desktop.stopCount() != 0 || harness.desktop.wslCount() != 0 {
		t.Fatal("Docker or WSL was shut down without every listener acknowledging zero capacity")
	}
}

func TestDeletedScaleSetIdentityIsRecreated(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Pools:         []model.PoolObservation{{ID: "org", ScaleSetID: 99, ListenerID: "deleted-listener"}},
	}); err != nil {
		t.Fatal(err)
	}
	harness.scaleSets.Errors["statistics:99"] = &scaleset.Error{Kind: scaleset.ErrorNotFound, StatusCode: 404, Operation: "statistics"}
	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Observed.Pools[0].ScaleSetID; got == 99 || got == 0 {
		t.Fatalf("scale set ID = %d, want recreated identity", got)
	}
}

func TestTransientScaleSetErrorUsesRetryPolicy(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	flaky := &flakyEnsure{Client: harness.scaleSets, remaining: 1}
	harness.controller.deps.ScaleSets = flaky
	policy := BackoffPolicy{
		Initial: time.Second, Maximum: 4 * time.Second, Multiplier: 2, MaxAttempts: 3,
		Jitter: func(base time.Duration, _ float64) time.Duration { return base },
	}
	if err := harness.controller.SetBackoffForTest(policy); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if flaky.attemptCount() != 2 {
		t.Fatalf("ensure attempts = %d", flaky.attemptCount())
	}
	if sleeps := harness.clock.Sleeps(); len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Fatalf("sleeps = %v", sleeps)
	}
}

func TestCorruptObservedStateQuarantinesAndAdvertisesZeroWithoutLifecycleMutation(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	badStore := &corruptObservedStore{Store: harness.store}
	harness.controller.deps.State = badStore
	result, err := harness.controller.Step(context.Background())
	if err == nil || !badStore.quarantined {
		t.Fatalf("error=%v quarantined=%v", err, badStore.quarantined)
	}
	foundZero := false
	for _, call := range harness.scaleSets.SnapshotCalls() {
		if call.Operation == "statistics" && call.MaxCapacity == 0 {
			foundZero = true
		}
	}
	if !foundZero || len(result.Observed.Pools) != 1 || !result.Observed.Pools[0].CapacityAcknowledged || result.Observed.Pools[0].MaxCapacity != 0 {
		t.Fatalf("corrupt recovery did not prove zero capacity: result=%#v calls=%#v", result, harness.scaleSets.SnapshotCalls())
	}
	if result.CheckpointAgeValid {
		t.Fatal("quarantined corrupt checkpoint reported a freshness age")
	}
	if harness.runtime.startCount() != 0 || len(result.Plan.Start) != 0 || len(result.Plan.Remove) != 0 {
		t.Fatalf("corrupt recovery performed lifecycle mutation: %#v", result.Plan)
	}
}

func TestMissingDesiredStateDefaultsDisabled(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	harness.store = statepkg.NewMemoryStore()
	harness.controller.deps.State = harness.store
	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desired, err := harness.store.LoadDesired(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desired.Mode != model.ModeDisabled || result.Observed.Pools[0].MaxCapacity != 0 {
		t.Fatalf("desired = %#v observed = %#v", desired, result.Observed)
	}
}

func TestControllerLogsNeverContainJITConfiguration(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.scaleSets.JIT = scaleset.NewRunnerJITConfig([]byte("DO-NOT-LOG-THIS-JIT"), 101)
	logger := &testLogSink{}
	harness.controller.deps.Logs = logger
	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logger.String(), "DO-NOT-LOG-THIS-JIT") {
		t.Fatal("JIT configuration appeared in structured controller events")
	}
}

func TestPreStartFailureDeregistersJITButAmbiguousStartDoesNot(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		startError error
		wantRemove bool
	}{
		{name: "pre-start", startError: &WorkerStartError{Err: errors.New("create failed")}, wantRemove: true},
		{name: "ambiguous-after-start", startError: &WorkerStartError{Err: errors.New("stream response lost"), RunnerMayBeActive: true}, wantRemove: false},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newHarness(t, model.ModeEnabled)
			harness.runtime.startErr = test.startError
			if _, err := harness.controller.Step(context.Background()); err == nil {
				t.Fatal("expected worker start failure")
			}
			removed := false
			for _, call := range harness.scaleSets.SnapshotCalls() {
				removed = removed || call.Operation == "remove-runner"
			}
			if removed != test.wantRemove {
				t.Fatalf("runner removed=%t, want %t; calls=%#v", removed, test.wantRemove, harness.scaleSets.SnapshotCalls())
			}
		})
	}
}

type testEngineMemory struct {
	mu    sync.Mutex
	total uint64
	err   error
	calls int
}

func (p *testEngineMemory) EngineMemoryTotal(context.Context) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return 0, p.err
	}
	return p.total, nil
}

func (p *testEngineMemory) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newProbeReconciler(t *testing.T, cfg config.Config, desktop *testDesktop, probe *testEngineMemory) *Reconciler {
	t.Helper()
	store := statepkg.NewMemoryStore()
	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	if err := store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	controller, err := NewReconciler(cfg, "test-version", Dependencies{
		ScaleSets:    scaleset.NewFake(),
		Workers:      &testRuntime{},
		Desktop:      desktop,
		Power:        staticPower{snapshot: model.PowerSnapshot{ACConnected: true, ObservedAt: now}},
		Resources:    staticResources{snapshot: model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 64 << 30, CPUUtilizationPercent: 10}},
		State:        store,
		Jobs:         &testJobLookup{active: map[string]string{}},
		Clock:        clockpkg.NewFake(now),
		EngineMemory: probe,
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func TestStepProbesEngineMemoryOncePerVMLifecycle(t *testing.T) {
	t.Parallel()
	desktop := &testDesktop{status: model.DesktopStatus{DesktopRunning: true, EngineReachable: true}}
	probe := &testEngineMemory{total: 8 << 30}
	cfg := validControllerConfig()
	cfg.Resources.WorkerMemoryBudget = config.ByteSize(36 << 30)
	controller := newProbeReconciler(t, cfg, desktop, probe)

	result, err := controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if probe.callCount() != 1 {
		t.Fatalf("probe calls after first step = %d, want 1", probe.callCount())
	}
	if !hasProblem(result.Observed.Problems, "worker-memory-budget-exceeds-engine-memory", "") {
		t.Fatalf("problems = %#v, want budget cross-check clamp against the 8GiB probe", result.Observed.Problems)
	}

	if _, err := controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probe.callCount() != 1 {
		t.Fatalf("probe calls after second step = %d, want cached 1", probe.callCount())
	}

	// A desktop teardown recycles the VM: the cached probe must not validate
	// the next VM, so a down step resets it and the next up step re-probes.
	desktop.mu.Lock()
	desktop.status = model.DesktopStatus{}
	desktop.startErr = errors.New("start blocked for the test")
	desktop.mu.Unlock()
	// The blocked desktop start surfaces as a step error by design; only the
	// probe accounting matters here.
	_, _ = controller.Step(context.Background())
	if probe.callCount() != 1 {
		t.Fatalf("probe calls while engine down = %d, want no probe", probe.callCount())
	}
	desktop.mu.Lock()
	desktop.startErr = nil
	desktop.mu.Unlock()
	if _, err := controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probe.callCount() != 2 {
		t.Fatalf("probe calls after engine recovery = %d, want re-probe 2", probe.callCount())
	}
}

func TestStepSkipsEngineMemoryProbeWithoutBudget(t *testing.T) {
	t.Parallel()
	desktop := &testDesktop{status: model.DesktopStatus{DesktopRunning: true, EngineReachable: true}}
	probe := &testEngineMemory{total: 8 << 30}
	controller := newProbeReconciler(t, validControllerConfig(), desktop, probe)

	if _, err := controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probe.callCount() != 0 {
		t.Fatalf("probe calls without a configured budget = %d, want 0", probe.callCount())
	}
}

type harness struct {
	controller *Reconciler
	store      *statepkg.MemoryStore
	runtime    *testRuntime
	desktop    *testDesktop
	scaleSets  *scaleset.Fake
	jobs       *testJobLookup
	clock      *clockpkg.Fake
}

func newHarness(t *testing.T, mode model.Mode) *harness {
	t.Helper()
	store := statepkg.NewMemoryStore()
	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	if err := store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: mode, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	runtime := &testRuntime{}
	desktop := &testDesktop{status: model.DesktopStatus{DesktopRunning: true, EngineReachable: true}}
	scaleSets := scaleset.NewFake()
	jobs := &testJobLookup{active: map[string]string{}}
	clock := clockpkg.NewFake(now)
	controller, err := NewReconciler(validControllerConfig(), "test-version", Dependencies{
		ScaleSets: scaleSets,
		Workers:   runtime,
		Desktop:   desktop,
		Power:     staticPower{snapshot: model.PowerSnapshot{ACConnected: true, ObservedAt: now}},
		Resources: staticResources{snapshot: model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 64 << 30, CPUUtilizationPercent: 10}},
		State:     store,
		Jobs:      jobs,
		Clock:     clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{controller: controller, store: store, runtime: runtime, desktop: desktop, scaleSets: scaleSets, jobs: jobs, clock: clock}
}

type testJobLookup struct {
	mu     sync.Mutex
	active map[string]string
	err    error
}

func (l *testJobLookup) ActiveJob(_ context.Context, poolID, runnerName string) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return "", false, l.err
	}
	jobID, found := l.active[poolID+"\x00"+runnerName]
	return jobID, found, nil
}

func validControllerConfig() config.Config {
	return config.Config{
		SchemaVersion: config.SupportedSchemaVersion,
		Host:          config.Host{ID: "melo-desk-001", RunnerNamePrefix: "melo-desk-001"},
		Controller: config.Controller{
			ReconcileInterval:    config.Duration{Duration: 5 * time.Second},
			ShutdownPollInterval: config.Duration{Duration: time.Second},
			LocalProbeTimeout:    config.Duration{Duration: 15 * time.Second},
			StartupTimeout:       config.Duration{Duration: 2 * time.Minute},
		},
		Release: config.Release{CompatibilityManifest: `C:\Users\runner\AppData\Local\ci-runner\release.json`},
		GitHub: config.GitHub{
			RequestTimeout: config.Duration{Duration: 70 * time.Second},
			Retry: config.Retry{
				Initial: config.Duration{Duration: time.Second}, Maximum: config.Duration{Duration: time.Minute},
				Multiplier: 2, JitterRatio: 0.2, MaxAttempts: 6,
			},
			Targets: []config.Target{{
				ID: "org", URL: "https://github.com/melodic-software", Scope: config.ScopeOrganization,
				ClientID: "Iv23liABCDEF1234", InstallationID: 12345, SecretID: "melodic-org-host", RunnerGroup: "ci-local-melo-desk-001",
				ScaleSetName: "melodic-ubuntu-24.04-x64", WarmIdle: 1, MaxCapacity: 3, Priority: 0,
			}},
		},
		Resources: config.Resources{
			MaximumConcurrentWorkers:  3,
			Worker:                    config.Worker{CPUs: 2, Memory: config.ByteSize(8 << 30), MemorySwap: config.ByteSize(8 << 30), PIDs: 4096},
			MinimumAvailableMemoryPct: 25, MemoryCapacityIncreaseMarginPct: 25, CPUBlockPercent: 75, CPUResumePercent: 60,
			CPUObservationWindow: config.Duration{Duration: time.Minute}, CPUHysteresisWindow: config.Duration{Duration: time.Minute},
		},
		Power: config.Power{Policy: config.PowerAlways, StableACWindow: config.Duration{Duration: 30 * time.Second}},
		Drain: config.Drain{
			WarningAfter:           config.Duration{Duration: 20 * time.Minute},
			IdleConfirmationWindow: config.Duration{Duration: 2 * time.Second},
		},
		DockerDesktop: config.DockerDesktop{StartTimeout: config.Duration{Duration: 2 * time.Minute}, StopTimeout: config.Duration{Duration: 2 * time.Minute}},
		WorkerImage:   config.WorkerImage{PullTimeout: config.Duration{Duration: 20 * time.Minute}},
		Logs: config.Logs{
			Docker:                    config.DockerLogs{Driver: "local", MaxSize: config.ByteSize(10 << 20), MaxFiles: 3},
			Controller:                config.LogClass{MaxFileSize: config.ByteSize(10 << 20), Retention: config.Duration{Duration: 14 * 24 * time.Hour}, TotalCap: config.ByteSize(512 << 20)},
			Diagnostics:               config.LogClass{MaxFileSize: config.ByteSize(100 << 20), Retention: config.Duration{Duration: 14 * 24 * time.Hour}, TotalCap: config.ByteSize(2 << 30)},
			RawDiagnosticMaxInput:     config.ByteSize(512 << 20),
			CleanupEvery:              config.Duration{Duration: 24 * time.Hour},
			WorkerFinalizationTimeout: config.Duration{Duration: 2 * time.Minute},
		},
		Paths: config.Paths{
			Secrets: `C:\Users\runner\AppData\Local\ci-runner\secrets`, State: `C:\Users\runner\AppData\Local\ci-runner\state`,
			Logs: `C:\Users\runner\AppData\Local\ci-runner\logs`, Diagnostics: `C:\Users\runner\AppData\Local\ci-runner\diagnostics`,
		},
	}
}

type testRuntime struct {
	mu              sync.Mutex
	workers         []model.Worker
	requests        []StartWorkerRequest
	starts          int
	lists           int
	listErr         error
	listErrAt       int
	forced          []string
	acquireOnRemove string
	removeErr       error
	trace           *callTrace
	closed          bool
	startErr        error
}

func (r *testRuntime) List(ctx context.Context) ([]model.Worker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lists++
	if r.trace != nil {
		r.trace.add("workers:list")
	}
	if r.listErr != nil && (r.listErrAt == 0 || r.lists == r.listErrAt) {
		return nil, r.listErr
	}
	result := append([]model.Worker(nil), r.workers...)
	for index := range result {
		if result[index].RunnerID == 0 {
			result[index].RunnerID = int64(index + 1000)
			r.workers[index].RunnerID = result[index].RunnerID
		}
	}
	return result, nil
}

func (r *testRuntime) Start(ctx context.Context, request StartWorkerRequest) (model.Worker, error) {
	if err := ctx.Err(); err != nil {
		return model.Worker{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	r.requests = append(r.requests, request)
	if r.startErr != nil {
		return model.Worker{}, r.startErr
	}
	worker := model.Worker{ID: request.Name, Name: request.Name, PoolID: request.PoolID, RunnerID: request.JITConfig.RunnerID(), State: model.WorkerIdle}
	r.workers = append(r.workers, worker)
	if r.trace != nil {
		r.trace.add("start:" + request.PoolID)
	}
	return worker, nil
}

func (r *testRuntime) RemoveIfIdle(ctx context.Context, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.workers {
		if r.workers[index].ID != id {
			continue
		}
		if r.removeErr != nil {
			return false, r.removeErr
		}
		if r.acquireOnRemove == id {
			r.workers[index].State = model.WorkerBusy
			r.workers[index].JobID = "raced-job"
			return false, nil
		}
		if r.workers[index].State == model.WorkerBusy {
			return false, nil
		}
		r.workers = append(r.workers[:index], r.workers[index+1:]...)
		if r.trace != nil {
			r.trace.add("remove:" + id)
		}
		return true, nil
	}
	return true, nil
}

func (r *testRuntime) ForceStop(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forced = append(r.forced, id)
	for index := range r.workers {
		if r.workers[index].ID == id {
			r.workers = append(r.workers[:index], r.workers[index+1:]...)
			break
		}
	}
	return nil
}

func (r *testRuntime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func (r *testRuntime) startCount() int { r.mu.Lock(); defer r.mu.Unlock(); return r.starts }
func (r *testRuntime) listCount() int  { r.mu.Lock(); defer r.mu.Unlock(); return r.lists }
func (r *testRuntime) startRequests() []StartWorkerRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]StartWorkerRequest(nil), r.requests...)
}
func (r *testRuntime) snapshot() []model.Worker {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.Worker(nil), r.workers...)
}
func (r *testRuntime) hasWorker(id string) bool {
	for _, worker := range r.snapshot() {
		if worker.ID == id {
			return true
		}
	}
	return false
}
func (r *testRuntime) forceStops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.forced...)
}
func (r *testRuntime) closedValue() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.closed }

type testDesktop struct {
	mu           sync.Mutex
	status       model.DesktopStatus
	startErr     error
	starts       int
	stops        int
	wslShutdowns int
	trace        *callTrace
}

func (d *testDesktop) Status(context.Context) (model.DesktopStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status, nil
}
func (d *testDesktop) Start(context.Context, time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts++
	if d.trace != nil {
		d.trace.add("desktop:start")
	}
	if d.startErr != nil {
		return d.startErr
	}
	d.status.DesktopRunning = true
	d.status.EngineReachable = true
	return nil
}
func (d *testDesktop) Stop(context.Context, time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stops++
	d.status.DesktopRunning = false
	d.status.EngineReachable = false
	return nil
}
func (d *testDesktop) ShutdownAllWSL(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.wslShutdowns++
	d.status.RunningWSLCount = 0
	return nil
}
func (d *testDesktop) stopCount() int { d.mu.Lock(); defer d.mu.Unlock(); return d.stops }
func (d *testDesktop) wslCount() int  { d.mu.Lock(); defer d.mu.Unlock(); return d.wslShutdowns }
func (d *testDesktop) startCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.starts
}
func (d *testDesktop) setStartError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.startErr = err
}

type staticPower struct {
	snapshot model.PowerSnapshot
	err      error
}

func (m staticPower) Snapshot(context.Context) (model.PowerSnapshot, error) { return m.snapshot, m.err }

type countingPower struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (p *countingPower) Snapshot(context.Context) (model.PowerSnapshot, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return model.PowerSnapshot{}, p.err
}
func (p *countingPower) callCount() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

func TestGenuinePollFailureRecordsStatisticsErrorWithUnderlyingCause(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	logs := &testLogSink{}
	harness.controller.deps.Logs = logs
	// A forbidden classification is non-retryable (no backoff loop) and does not
	// trigger the not-found recreate path, so the drain loop records it directly.
	harness.scaleSets.Errors["statistics:1"] = &scaleset.Error{
		Kind: scaleset.ErrorForbidden, StatusCode: 403, Operation: "statistics",
		Err: errors.New(`activity_id="act-123" github_request_id="req-xyz"`),
	}

	result, _ := harness.controller.Step(context.Background())

	assertProblemCode(t, result.Observed.Problems, "scale-set-statistics-error")
	if result.Observed.Phase != model.PhaseDegraded {
		t.Fatalf("genuine poll failure did not degrade the phase: %#v", result.Observed)
	}
	event, found := logs.find("scale-set-statistics-error")
	if !found {
		t.Fatalf("genuine poll failure was not logged: %s", logs)
	}
	if !strings.Contains(event.Message, "HTTP 403") {
		t.Fatalf("poll failure message dropped the classified status: %q", event.Message)
	}
	if !strings.Contains(event.Cause, "github_request_id") || !strings.Contains(event.Cause, "req-xyz") {
		t.Fatalf("poll failure log dropped the underlying GitHub detail: %q", event.Cause)
	}
}

func TestPollSupersededConsultsOnlyTheResultError(t *testing.T) {
	t.Parallel()
	// With multiple ready pools, the cadence watcher's cancellation sets the step
	// cause for every queued result; a genuine failure from another pool must not
	// inherit benign-supersession treatment from that shared cause.
	if pollSuperseded(&scaleset.Error{Kind: scaleset.ErrorTransport, Operation: "statistics", Err: errors.New("connection reset")}) {
		t.Fatal("a genuine scale-set failure was classified as a benign supersession")
	}
	if !pollSuperseded(context.Canceled) {
		t.Fatal("a canceled in-flight poll was not classified as a supersession")
	}
	if !pollSuperseded(fmt.Errorf("poll: %w", context.Canceled)) {
		t.Fatal("a wrapped cancellation was not classified as a supersession")
	}
}

type delayedScaleSet struct {
	scaleset.Client
	mu    sync.Mutex
	calls int
	delay time.Duration
}

func (s *delayedScaleSet) Statistics(ctx context.Context, identity scaleset.Identity, maximum int) (scaleset.Statistics, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return scaleset.Statistics{}, ctx.Err()
	case <-timer.C:
		return s.Client.Statistics(ctx, identity, maximum)
	}
}
func (s *delayedScaleSet) callCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

type staticResources struct {
	snapshot model.ResourceSnapshot
	err      error
}

func (m staticResources) Snapshot(context.Context) (model.ResourceSnapshot, error) {
	return m.snapshot, m.err
}

type sequenceResources struct {
	mu        sync.Mutex
	snapshots []model.ResourceSnapshot
	next      int
}

func (m *sequenceResources) Snapshot(context.Context) (model.ResourceSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.snapshots) == 0 {
		return model.ResourceSnapshot{}, errors.New("test resource sequence is empty")
	}
	index := m.next
	if index >= len(m.snapshots) {
		index = len(m.snapshots) - 1
	} else {
		m.next++
	}
	return m.snapshots[index], nil
}

type callTrace struct {
	mu      sync.Mutex
	entries []string
}

func (t *callTrace) add(value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = append(t.entries, value)
}
func (t *callTrace) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.entries...)
}

type tracingScaleSet struct {
	scaleset.Client
	trace *callTrace
}

func (s *tracingScaleSet) Statistics(ctx context.Context, identity scaleset.Identity, max int) (scaleset.Statistics, error) {
	s.trace.add(fmt.Sprintf("statistics:%d", max))
	return s.Client.Statistics(ctx, identity, max)
}
func (s *tracingScaleSet) RemoveRunner(ctx context.Context, poolID string, runnerID int64) error {
	s.trace.add(fmt.Sprintf("deregister:%s:%d", poolID, runnerID))
	return s.Client.RemoveRunner(ctx, poolID, runnerID)
}

// runnerRemovalFailureClient wraps a scaleset.Client and returns a
// persistent error for RemoveRunner calls against one specific runner ID,
// delegating every other call (including RemoveRunner for other runner IDs)
// to the wrapped client. It simulates one worker's deregistration being
// permanently stuck without needing a Fake field, for
// TestWorkerRetirementRotatesStartingWorkerToAvoidHeadOfLineBlocker.
type runnerRemovalFailureClient struct {
	scaleset.Client
	failRunnerID int64
	err          error
}

func (s *runnerRemovalFailureClient) RemoveRunner(ctx context.Context, poolID string, runnerID int64) error {
	if runnerID == s.failRunnerID {
		return s.err
	}
	return s.Client.RemoveRunner(ctx, poolID, runnerID)
}

type flakyEnsure struct {
	scaleset.Client
	mu        sync.Mutex
	remaining int
	attempts  int
}

func (s *flakyEnsure) Ensure(ctx context.Context, definition scaleset.Definition, previous *scaleset.Identity) (scaleset.Identity, error) {
	s.mu.Lock()
	s.attempts++
	shouldFail := s.remaining > 0
	if shouldFail {
		s.remaining--
	}
	s.mu.Unlock()
	if shouldFail {
		return scaleset.Identity{}, &scaleset.Error{Kind: scaleset.ErrorRateLimited, StatusCode: 429, RetryAfterSeconds: 1}
	}
	return s.Client.Ensure(ctx, definition, previous)
}
func (s *flakyEnsure) attemptCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.attempts }

type firstBlockingScaleSet struct {
	scaleset.Client
	entered    chan struct{}
	once       sync.Once
	mu         sync.Mutex
	calls      int
	capacities []int
}

type firstBlockingEnsure struct {
	scaleset.Client
	entered chan struct{}
	mu      sync.Mutex
	calls   int
}

func newFirstBlockingEnsure(client scaleset.Client) *firstBlockingEnsure {
	return &firstBlockingEnsure{Client: client, entered: make(chan struct{})}
}

func (s *firstBlockingEnsure) Ensure(ctx context.Context, definition scaleset.Definition, previous *scaleset.Identity) (scaleset.Identity, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	if call == 1 {
		close(s.entered)
	}
	s.mu.Unlock()
	if call == 1 {
		<-ctx.Done()
		return scaleset.Identity{}, ctx.Err()
	}
	return s.Client.Ensure(ctx, definition, previous)
}

func newFirstBlockingScaleSet(client scaleset.Client) *firstBlockingScaleSet {
	return &firstBlockingScaleSet{Client: client, entered: make(chan struct{})}
}

func (s *firstBlockingScaleSet) Statistics(ctx context.Context, identity scaleset.Identity, max int) (scaleset.Statistics, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.capacities = append(s.capacities, max)
	s.mu.Unlock()
	if call == 1 {
		s.once.Do(func() { close(s.entered) })
		<-ctx.Done()
		return scaleset.Statistics{}, ctx.Err()
	}
	return s.Client.Statistics(ctx, identity, max)
}

func (s *firstBlockingScaleSet) capacitiesSnapshot() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.capacities...)
}

type mutablePower struct {
	mu       sync.Mutex
	snapshot model.PowerSnapshot
}

type blockingDesktop struct {
	mu      sync.Mutex
	status  model.DesktopStatus
	entered chan struct{}
	once    sync.Once
}

func newBlockingDesktop() *blockingDesktop {
	return &blockingDesktop{entered: make(chan struct{})}
}

func (d *blockingDesktop) Status(context.Context) (model.DesktopStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status, nil
}

func (d *blockingDesktop) Start(ctx context.Context, _ time.Duration) error {
	d.once.Do(func() { close(d.entered) })
	<-ctx.Done()
	return ctx.Err()
}

func (d *blockingDesktop) Stop(context.Context, time.Duration) error { return nil }
func (d *blockingDesktop) ShutdownAllWSL(context.Context) error      { return nil }

func (p *mutablePower) Snapshot(context.Context) (model.PowerSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshot, nil
}

func (p *mutablePower) set(connected bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot.ACConnected = connected
}

type desiredFlippingScaleSet struct {
	scaleset.Client
	store StateStore
	now   time.Time
	once  sync.Once
}

func (s *desiredFlippingScaleSet) CreateJITConfig(ctx context.Context, identity scaleset.Identity, runnerName string) (scaleset.JITConfig, error) {
	jit, err := s.Client.CreateJITConfig(ctx, identity, runnerName)
	if err != nil {
		return jit, err
	}
	var saveErr error
	s.once.Do(func() {
		saveErr = s.store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled, UpdatedAt: s.now})
	})
	return jit, saveErr
}

func waitForSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

type corruptObservedStore struct {
	statepkg.Store
	quarantined bool
}

func (*corruptObservedStore) LoadObserved(context.Context) (model.ObservedState, error) {
	return model.ObservedState{}, errors.New("corrupt JSON")
}

func (s *corruptObservedStore) QuarantineObserved(context.Context) error {
	s.quarantined = true
	return nil
}

type testLogSink struct {
	mu            sync.Mutex
	events        []LogEvent
	contexts      []context.Context
	contextErrors []error
	deadlines     []bool
}

func (s *testLogSink) Write(ctx context.Context, event LogEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	s.contexts = append(s.contexts, ctx)
	s.contextErrors = append(s.contextErrors, ctx.Err())
	_, hasDeadline := ctx.Deadline()
	s.deadlines = append(s.deadlines, hasDeadline)
	return nil
}

func TestWriteLogDetachesCancellationAndPreservesValues(t *testing.T) {
	harness := newHarness(t, model.ModeEnabled)
	logs := &testLogSink{}
	harness.controller.deps.Logs = logs
	type traceKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), traceKey{}, "trace-123"))
	cancel()

	harness.controller.writeLog(ctx, LogEvent{Code: "cancellation-diagnostic"})

	logs.mu.Lock()
	defer logs.mu.Unlock()
	if len(logs.contexts) != 1 {
		t.Fatalf("log contexts = %d, want 1", len(logs.contexts))
	}
	if err := logs.contextErrors[0]; err != nil {
		t.Fatalf("diagnostic context was canceled during the sink call: %v", err)
	}
	if !logs.deadlines[0] {
		t.Fatal("diagnostic context has no bounded write deadline")
	}
	if got := logs.contexts[0].Value(traceKey{}); got != "trace-123" {
		t.Fatalf("diagnostic context value = %v, want trace-123", got)
	}
}
func (s *testLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("%#v", s.events)
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func assertProblemCode(t *testing.T, problems []model.Problem, code string) {
	t.Helper()
	for _, problem := range problems {
		if problem.Code == code {
			return
		}
	}
	t.Fatalf("problem %q not found in %#v", code, problems)
}
