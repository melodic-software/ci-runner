// Package controller contains the platform-neutral reconciliation policy.
package controller

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/scaleset"
)

type PoolSnapshot struct {
	TargetID             string
	Identity             scaleset.Identity
	TotalAssignedJobs    int
	DrainServiceCapacity int
	Ready                bool
}

type PlanInput struct {
	Config   config.Config
	Desired  model.DesiredState
	Previous model.ObservedState
	// CapacityHysteresis overrides the last acknowledged capacity only for
	// memory Schmitt-trigger state. During a long poll, this is the capacity
	// currently in flight; it must hold inside the dead band without being
	// misrepresented as acknowledged in the durable observation.
	CapacityHysteresis map[string]int
	Pools              []PoolSnapshot
	Workers            []model.Worker
	Resources          model.ResourceSnapshot
	Power              model.PowerSnapshot
	Desktop            model.DesktopStatus
	// EngineMemoryTotalBytes is the probed total memory of the VM backing the
	// Docker engine (WSL2 on Windows), or zero when unknown. It only
	// cross-checks a configured WorkerMemoryBudget: a budget larger than the
	// VM promises memory the workers' kernel does not have, so the effective
	// budget clamps to the probe.
	EngineMemoryTotalBytes uint64
	Now                    time.Time
}

type StartDecision struct {
	PoolID string
	Count  int
}

type Plan struct {
	Phase              model.Phase
	AdvertisedCapacity map[string]int
	DesiredWorkers     map[string]int
	Start              []StartDecision
	Remove             []model.Worker
	StartDesktop       bool
	StopDesktop        bool
	ShutdownWSL        bool
	ResourceGate       model.ResourceGateState
	PowerGate          model.PowerGateState
	Problems           []model.Problem
	// MemoryHeadroom is the memory left unspent by this plan under whichever
	// basis was active (static worker-memory budget, or legacy host physical
	// headroom). MemoryAffordable is the additional worker count that
	// remainder funds per pool at its effective worker profile. MemoryClamped
	// marks pools whose starts or advertised capacity the memory term (or the
	// host-floor backstop) bound below what host and pool limits allowed --
	// the previously silent clamp condition.
	MemoryHeadroom   uint64
	MemoryAffordable map[string]int
	MemoryClamped    map[string]bool
}

// EffectiveMaximumConcurrentWorkers resolves the host-wide worker cap
// BuildPlan enforces for a reconcile: the desired state's
// TemporaryCapacityOverride when an operator has set one, otherwise the
// static configured Resources.MaximumConcurrentWorkers. Exported so callers
// outside this package (internal/app's reconcile-step watchdog) can size a
// budget against the same effective limit BuildPlan actually applies to
// worker starts, instead of just the static cap -- a legitimate temporary
// scale-up (override greater than the static cap) must not trip the watchdog
// on a policy-compliant burst reconcile it correctly authorized.
func EffectiveMaximumConcurrentWorkers(resources config.Resources, desired model.DesiredState) int {
	if desired.TemporaryCapacityOverride != nil {
		return *desired.TemporaryCapacityOverride
	}
	return resources.MaximumConcurrentWorkers
}

// BuildPlan is pure: it computes a complete desired transition without
// invoking an adapter. Lower target priority values are allocated first.
func BuildPlan(input PlanInput) Plan {
	plan := Plan{
		Phase:              model.PhaseStarting,
		AdvertisedCapacity: make(map[string]int, len(input.Config.GitHub.Targets)),
		DesiredWorkers:     make(map[string]int, len(input.Config.GitHub.Targets)),
		MemoryAffordable:   make(map[string]int, len(input.Config.GitHub.Targets)),
		MemoryClamped:      make(map[string]bool, len(input.Config.GitHub.Targets)),
	}
	for _, target := range input.Config.GitHub.Targets {
		plan.AdvertisedCapacity[target.ID] = 0
		plan.DesiredWorkers[target.ID] = 0
		plan.MemoryAffordable[target.ID] = 0
		plan.MemoryClamped[target.ID] = false
	}
	if input.Now.IsZero() {
		input.Now = time.Now().UTC()
	}

	var resourceHealthy bool
	var resourceProblem string
	plan.ResourceGate, resourceHealthy, resourceProblem = evaluateResourceGate(
		input.Previous.ResourceGate,
		input.Resources,
		input.Config.Resources,
		input.Now,
	)
	if resourceProblem != "" {
		plan.Problems = append(plan.Problems, problem(input.Now, "invalid-resource-observation", resourceProblem, "", true))
	}
	var powerAllowed bool
	plan.PowerGate, powerAllowed = evaluatePowerGate(
		input.Previous.PowerGate,
		input.Power,
		input.Config.Power,
		input.Now,
	)

	workersByPool := make(map[string][]model.Worker, len(input.Config.GitHub.Targets))
	targetIDs := make(map[string]struct{}, len(input.Config.GitHub.Targets))
	busyWorkers := 0
	activeWorkers := 0
	for _, target := range input.Config.GitHub.Targets {
		targetIDs[target.ID] = struct{}{}
	}
	for _, worker := range input.Workers {
		workersByPool[worker.PoolID] = append(workersByPool[worker.PoolID], worker)
		if worker.State == model.WorkerBusy {
			busyWorkers++
		}
		if worker.Active() {
			activeWorkers++
		}
		if _, known := targetIDs[worker.PoolID]; !known {
			switch worker.State {
			case model.WorkerExited, model.WorkerUnregistered:
				plan.Remove = append(plan.Remove, worker)
			case model.WorkerBusy:
				plan.Problems = append(plan.Problems, problem(input.Now, "orphaned-busy-worker", "busy worker belongs to an unknown pool and will be preserved", worker.PoolID, false))
			default:
				plan.Problems = append(plan.Problems, problem(input.Now, "orphaned-worker-retirement-deferred", "active worker belongs to an unknown pool and cannot be quiesced safely; it will be preserved", worker.PoolID, false))
			}
		}
	}
	if !input.Desired.Mode.Valid() {
		plan.Phase = model.PhaseDegraded
		plan.Problems = append(plan.Problems, problem(input.Now, "invalid-desired-mode", fmt.Sprintf("unsupported desired mode %q", input.Desired.Mode), "", false))
		applyOutstandingAssignments(&plan, input, workersByPool, targetIDs, activeWorkers)
		return plan
	}

	switch input.Desired.Mode {
	case model.ModeDisabled:
		outstanding := applyOutstandingAssignments(&plan, input, workersByPool, targetIDs, activeWorkers)
		if hasActiveWorkers(input.Workers) || outstanding || len(plan.Start) > 0 {
			plan.Phase = model.PhaseDraining
		} else {
			plan.Phase = model.PhaseDisabled
		}
		return plan

	case model.ModeGaming:
		outstanding := applyOutstandingAssignments(&plan, input, workersByPool, targetIDs, activeWorkers)
		if busyWorkers > 0 || outstanding || len(plan.Start) > 0 {
			plan.Phase = model.PhaseDraining
			return plan
		}
		plan.StopDesktop = input.Desktop.DesktopRunning || input.Desktop.EngineReachable
		plan.ShutdownWSL = input.Desktop.RunningWSLCount > 0
		if plan.StopDesktop || plan.ShutdownWSL || hasActiveWorkers(input.Workers) {
			plan.Phase = model.PhaseDraining
		} else {
			plan.Phase = model.PhaseGaming
		}
		return plan
	}

	if !powerAllowed {
		applyOutstandingAssignments(&plan, input, workersByPool, targetIDs, activeWorkers)
		plan.Phase = model.PhasePowerSuspended
		return plan
	}
	// Docker Desktop is a host prerequisite, not a worker-admission decision, so
	// its start must precede the resource gate: a blocked gate (for example the
	// invalid observation that intentional teardown produces) must never prevent
	// the start that re-enable depends on. The gate still fails closed for worker
	// scheduling below once the desktop is up.
	if !input.Desktop.DesktopRunning || !input.Desktop.EngineReachable {
		plan.StartDesktop = true
		plan.Phase = model.PhaseStarting
		return plan
	}
	if !resourceHealthy {
		applyOutstandingAssignments(&plan, input, workersByPool, targetIDs, activeWorkers)
		plan.Phase = model.PhaseResourceConstrained
		return plan
	}

	poolByID := make(map[string]PoolSnapshot, len(input.Pools))
	for _, pool := range input.Pools {
		poolByID[pool.TargetID] = pool
	}
	previousCapacityByPool := make(map[string]int, len(input.Previous.Pools))
	for _, pool := range input.Previous.Pools {
		previousCapacityByPool[pool.ID] = pool.MaxCapacity
	}
	for poolID, capacity := range input.CapacityHysteresis {
		previousCapacityByPool[poolID] = capacity
	}
	targets := sortedTargetsByPriority(input.Config.GitHub.Targets)

	hostLimit := EffectiveMaximumConcurrentWorkers(input.Config.Resources, input.Desired)
	if hostLimit < 0 {
		plan.Phase = model.PhaseDegraded
		plan.Problems = append(plan.Problems, problem(input.Now, "invalid-capacity-override", "temporary capacity override must not be negative", "", false))
		appendSafeRemovals(&plan, workersByPool, targetIDs, nil)
		return plan
	}

	// Busy workers are immutable reservations. They consume global capacity
	// before any target receives assigned-job or warm-idle capacity.
	remaining := hostLimit - busyWorkers
	gate := evaluateMemoryBasis(input)
	if gate.budgetClamped {
		plan.Problems = append(plan.Problems, problem(input.Now, "worker-memory-budget-exceeds-engine-memory", "configured workerMemoryBudget exceeds the probed engine VM memory; the effective budget is clamped to the probe", "", false))
	}
	memoryRemaining := gate.remaining
	if remaining < 0 {
		remaining = 0
		plan.Problems = append(plan.Problems, problem(input.Now, "capacity-below-busy-count", "configured host capacity is below the current busy-worker count; active work is preserved and no new work is admitted", "", false))
	}
	busyByPool := make(map[string]int, len(targets))
	nonbusyByPool := make(map[string]int, len(targets))
	countedNonbusy := make(map[string]int, len(targets))
	for _, target := range targets {
		for _, worker := range workersByPool[target.ID] {
			switch worker.State {
			case model.WorkerBusy:
				plan.DesiredWorkers[target.ID]++
				busyByPool[target.ID]++
			case model.WorkerStarting, model.WorkerIdle:
				nonbusyByPool[target.ID]++
			}
		}
	}

	ready := make(map[string]bool, len(targets))
	assignmentReservations := make(map[string]int, len(targets))
	assignmentOverCap := make(map[string]bool, len(targets))
	for _, target := range targets {
		pool, exists := poolByID[target.ID]
		if !exists || !pool.Ready {
			plan.Problems = append(plan.Problems, problem(input.Now, "pool-unavailable", "scale-set listener is unavailable; capacity is held at zero", target.ID, true))
			continue
		}
		assigned := pool.TotalAssignedJobs
		if assigned < 0 {
			assigned = 0
			plan.Problems = append(plan.Problems, problem(input.Now, "invalid-assigned-job-count", "GitHub reported a negative TotalAssignedJobs value", target.ID, true))
		}
		ready[target.ID] = true
		assignmentReservations[target.ID] = min(assigned, target.MaxCapacity)
		if assigned > target.MaxCapacity {
			assignmentOverCap[target.ID] = true
			plan.Problems = append(plan.Problems, problem(input.Now, "assigned-job-count-exceeds-pool-cap", "GitHub reported more assigned jobs than the configured pool maximum; excess assignments are not reserved and listener capacity is held at zero", target.ID, true))
		}
	}

	allocate := func(target config.Target, requested int) {
		current := plan.DesiredWorkers[target.ID]
		if requested <= current {
			return
		}
		need := requested - current
		availableExisting := nonbusyByPool[target.ID] - countedNonbusy[target.ID]
		existing := min(need, min(availableExisting, remaining))
		plan.DesiredWorkers[target.ID] += existing
		countedNonbusy[target.ID] += existing
		remaining -= existing

		prospective := min(need-existing, remaining)
		workerMemory := target.EffectiveWorker(input.Config.Resources.Worker).Memory
		affordable := affordableWorkerCount(memoryRemaining, workerMemory)
		if gate.floorBlocked {
			affordable = 0
		}
		starts := min(prospective, affordable)
		if starts < prospective {
			plan.MemoryClamped[target.ID] = true
		}
		plan.DesiredWorkers[target.ID] += starts
		remaining -= starts
		memoryRemaining -= uint64(starts) * uint64(workerMemory)
	}

	// Reserve every authoritative assignment before allocating a single warm
	// idle worker. Capacity is advertised across independent scale-set
	// listeners, so a high-priority pool's idle preference must never consume a
	// slot already assigned to another ready pool.
	for _, target := range targets {
		if !ready[target.ID] {
			continue
		}
		assigned := max(assignmentReservations[target.ID], busyByPool[target.ID])
		allocate(target, assigned)
		if plan.DesiredWorkers[target.ID] < assigned {
			plan.Problems = append(plan.Problems, problem(input.Now, "assigned-job-capacity-unavailable", "GitHub has assigned work that exceeds currently serviceable host capacity", target.ID, true))
		}
	}

	// Priority applies only to optional warm-idle inventory after all assigned
	// work has been reserved.
	for _, target := range targets {
		if !ready[target.ID] {
			continue
		}
		assigned := assignmentReservations[target.ID]
		requested := min(target.MaxCapacity, assigned+target.WarmIdle)
		requested = max(requested, max(assigned, busyByPool[target.ID]))
		allocate(target, requested)
	}

	capacityDebt := busyWorkers > hostLimit
	quiescing := false
	advertisable := make(map[string]bool, len(targets))
	type burstCandidate struct {
		poolID      string
		maxCapacity int
		removable   []model.Worker
	}
	burstCandidates := make([]burstCandidate, 0, len(targets))
	for _, target := range targets {
		desired := plan.DesiredWorkers[target.ID]
		active := 0
		removable := make([]model.Worker, 0)
		for _, worker := range workersByPool[target.ID] {
			switch worker.State {
			case model.WorkerBusy:
				active++
			case model.WorkerStarting, model.WorkerIdle:
				active++
				removable = append(removable, worker)
			case model.WorkerExited, model.WorkerUnregistered:
				plan.Remove = append(plan.Remove, worker)
			}
		}
		if active < desired {
			plan.Start = append(plan.Start, StartDecision{PoolID: target.ID, Count: desired - active})
		}
		// A single excess registered worker is useful burst inventory. Keep it
		// eligible instead of taking the entire pool to zero just to retire one
		// runner. The next job can consume that ephemeral runner and converge the
		// pool naturally without a deregistration race or availability blackout.
		//
		// Larger or explicit zero-capacity downscales still quiesce. Reconciler
		// requires two authoritative zero-assignment polls plus durable
		// no-active-job evidence before any selected worker is deregistered and
		// removed.
		if active > desired {
			excess := active - desired
			if excess == 1 && hostLimit > 0 && target.MaxCapacity > 0 && !capacityDebt && ready[target.ID] && !assignmentOverCap[target.ID] {
				// Begin with this pool's globally allocated worker demand. A later
				// pass represents the retained excess worker only if capacity remains
				// in the single host-wide advertisement budget.
				plan.AdvertisedCapacity[target.ID] = desired
				advertisable[target.ID] = true
				burstCandidates = append(burstCandidates, burstCandidate{
					poolID: target.ID, maxCapacity: target.MaxCapacity, removable: removable,
				})
			} else {
				quiescing = true
				for _, worker := range removable {
					if excess == 0 {
						break
					}
					plan.Remove = append(plan.Remove, worker)
					excess--
				}
			}
		} else if !capacityDebt && ready[target.ID] && !assignmentOverCap[target.ID] {
			plan.AdvertisedCapacity[target.ID] = desired
			advertisable[target.ID] = true
		}
	}

	// Desired worker allocation is globally bounded, but each retained excess
	// worker belongs to an independent listener. Representing every excess in
	// its pool capacity without a shared budget can therefore over-advertise the
	// host. Preserve zero-warm pools first so they can converge naturally, then
	// spend any remaining budget in target priority order. A zero-warm pool that
	// cannot receive even one safe slot falls back to exact-runner quiescence.
	advertisedBudget := hostLimit
	for _, capacity := range plan.AdvertisedCapacity {
		advertisedBudget -= capacity
	}
	if advertisedBudget < 0 {
		advertisedBudget = 0
	}
	sort.SliceStable(burstCandidates, func(i, j int) bool {
		leftZero := plan.AdvertisedCapacity[burstCandidates[i].poolID] == 0
		rightZero := plan.AdvertisedCapacity[burstCandidates[j].poolID] == 0
		return leftZero && !rightZero
	})
	for _, candidate := range burstCandidates {
		capacity := plan.AdvertisedCapacity[candidate.poolID]
		if capacity < candidate.maxCapacity && advertisedBudget > 0 {
			plan.AdvertisedCapacity[candidate.poolID]++
			advertisedBudget--
			capacity++
		}
		if capacity != 0 {
			continue
		}
		quiescing = true
		advertisable[candidate.poolID] = false
		if len(candidate.removable) > 0 {
			plan.Remove = append(plan.Remove, candidate.removable[0])
		}
	}

	// GitHub's scale-set listener capacity is the maximum number of assigned
	// jobs the host can service, not the number of workers that should exist
	// before assignment. ARC advertises maxRunners to the listener and computes
	// its actual worker count separately as minRunners+assigned. Preserve that
	// split here: desired workers remain assigned+warm-idle, while additional
	// host- and memory-bounded slots are advertised without prestarting them.
	// A gate, capacity debt, or per-pool quiesce still leaves capacity at zero.
	if !capacityDebt {
		serviceSlots := hostLimit - activeWorkers
		for _, decision := range plan.Start {
			serviceSlots -= decision.Count
		}
		if serviceSlots < 0 {
			serviceSlots = 0
		}
		for _, target := range targets {
			if !advertisable[target.ID] || serviceSlots == 0 {
				continue
			}
			currentCapacity := plan.AdvertisedCapacity[target.ID]
			additionalLimit := min(target.MaxCapacity-currentCapacity, serviceSlots)
			additionalLimit = min(additionalLimit, advertisedBudget)
			workerMemory := target.EffectiveWorker(input.Config.Resources.Worker).Memory
			rawAffordable := affordableWorkerCount(memoryRemaining, workerMemory)
			// Memory-backed capacity decreases immediately at the raw slot
			// boundary. Growth requires extra headroom, while an already
			// advertised slot remains stable inside that Schmitt-trigger band.
			held := min(additionalLimit, max(previousCapacityByPool[target.ID]-currentCapacity, 0))
			held = min(held, rawAffordable)
			memoryAfterHeld := memoryRemaining - uint64(held)*uint64(workerMemory)
			growth := min(
				additionalLimit-held,
				affordableWorkerCountWithMargin(
					memoryAfterHeld,
					workerMemory,
					input.Config.Resources.MemoryCapacityIncreaseMarginPct,
				),
			)
			// The floor backstop stops advertised growth but never withdraws
			// already-acknowledged (held) capacity: withdrawal under a transient
			// host-memory dip would flap listeners the Schmitt trigger exists to
			// keep stable.
			if gate.floorBlocked {
				growth = 0
			}
			additional := held + growth
			if additional < additionalLimit {
				plan.MemoryClamped[target.ID] = true
			}
			if additional <= 0 {
				continue
			}
			plan.AdvertisedCapacity[target.ID] += additional
			serviceSlots -= additional
			advertisedBudget -= additional
			memoryRemaining -= uint64(additional) * uint64(workerMemory)
		}
	}
	plan.MemoryHeadroom = memoryRemaining
	for _, target := range targets {
		plan.MemoryAffordable[target.ID] = affordableWorkerCount(memoryRemaining, target.EffectiveWorker(input.Config.Resources.Worker).Memory)
	}
	if len(plan.Problems) > 0 {
		plan.Phase = model.PhaseDegraded
	} else if quiescing {
		plan.Phase = model.PhaseDraining
	} else {
		plan.Phase = model.PhaseReady
	}
	return plan
}

// applyOutstandingAssignments closes the gap between the official client
// acquiring a job and the worker hook reporting "busy". Capacity remains zero,
// but existing nonbusy workers are retained and missing one-job workers are
// started up to the last nonzero acknowledged capacity that could have caused
// the assignment.
func applyOutstandingAssignments(plan *Plan, input PlanInput, workersByPool map[string][]model.Worker, known map[string]struct{}, activeWorkers int) bool {
	poolByID := make(map[string]PoolSnapshot, len(input.Pools))
	reservations := make(map[string]int, len(input.Pools))
	outstanding := false
	for _, pool := range input.Pools {
		poolByID[pool.TargetID] = pool
		if _, ok := known[pool.TargetID]; !ok || !pool.Ready || pool.TotalAssignedJobs <= 0 {
			continue
		}
		busy := 0
		for _, worker := range workersByPool[pool.TargetID] {
			if worker.State == model.WorkerBusy {
				busy++
			}
		}
		if uncovered := pool.TotalAssignedJobs - busy; uncovered > 0 {
			reservations[pool.TargetID] = uncovered
			outstanding = true
		}
	}
	appendSafeRemovals(plan, workersByPool, known, reservations)

	remaining := input.Config.Resources.MaximumConcurrentWorkers - activeWorkers
	if remaining < 0 {
		remaining = 0
	}
	gate := evaluateMemoryBasis(input)
	memoryRemaining := gate.remaining
	// The static budget stays authoritative even when the host observation is
	// invalid; the legacy host basis must not treat its synthetic zero headroom
	// as authoritative for work GitHub already assigned.
	memoryObservationValid := gate.budgetActive ||
		(input.Resources.TotalMemoryBytes > 0 &&
			input.Resources.AvailableMemoryBytes <= input.Resources.TotalMemoryBytes)
	targets := sortedTargetsByPriority(input.Config.GitHub.Targets)
	for _, target := range targets {
		pool := poolByID[target.ID]
		uncovered := reservations[target.ID]
		busy, nonbusy := 0, 0
		for _, worker := range workersByPool[target.ID] {
			switch worker.State {
			case model.WorkerBusy:
				busy++
			case model.WorkerStarting, model.WorkerIdle:
				nonbusy++
			}
		}
		serviceable := min(uncovered, min(pool.DrainServiceCapacity, target.MaxCapacity))
		workerMemory := target.EffectiveWorker(input.Config.Resources.Worker).Memory
		start := min(max(0, serviceable-nonbusy), remaining)
		// An assignment can win the race with a fail-closed poll cancellation.
		// When physical-memory telemetry is valid, keep using the exact target
		// profile to rank what can start now. When it is invalid, do not treat
		// the synthetic zero headroom as authoritative for work GitHub already
		// assigned under the last acknowledged, memory-bounded capacity.
		if memoryObservationValid {
			start = min(start, affordableWorkerCount(memoryRemaining, workerMemory))
		}
		if gate.floorBlocked {
			start = 0
		}
		if start > 0 {
			plan.Start = append(plan.Start, StartDecision{PoolID: target.ID, Count: start})
			remaining -= start
			if memoryObservationValid {
				memoryRemaining -= uint64(start) * uint64(workerMemory)
			}
			if !input.Desktop.DesktopRunning || !input.Desktop.EngineReachable {
				plan.StartDesktop = true
			}
		}
		plan.DesiredWorkers[target.ID] = busy + min(uncovered, nonbusy+start)
		if uncovered > nonbusy+start {
			plan.Problems = append(plan.Problems, problem(input.Now, "assigned-job-capacity-unavailable", "GitHub has assigned work that exceeds the last acknowledged service capacity; capacity remains zero and existing workers are preserved", target.ID, true))
		}
	}
	return outstanding
}

func appendSafeRemovals(plan *Plan, workersByPool map[string][]model.Worker, known map[string]struct{}, desired map[string]int) {
	for poolID, workers := range workersByPool {
		if _, ok := known[poolID]; !ok {
			continue // unknown nonbusy workers were already appended exactly once
		}
		keep := 0
		if desired != nil {
			keep = desired[poolID]
		}
		activeNonBusy := 0
		for _, worker := range workers {
			if worker.State == model.WorkerStarting || worker.State == model.WorkerIdle {
				activeNonBusy++
			}
		}
		removeCount := activeNonBusy - keep
		for _, worker := range workers {
			if worker.State == model.WorkerExited || worker.State == model.WorkerUnregistered {
				plan.Remove = append(plan.Remove, worker)
				continue
			}
			if removeCount > 0 && (worker.State == model.WorkerStarting || worker.State == model.WorkerIdle) {
				plan.Remove = append(plan.Remove, worker)
				removeCount--
			}
		}
	}
}

// sortedTargetsByPriority returns a copy of targets ordered by ascending
// priority, breaking ties by ID for deterministic allocation order. Lower
// priority values are allocated first.
func sortedTargetsByPriority(targets []config.Target) []config.Target {
	sorted := append([]config.Target(nil), targets...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Priority == sorted[j].Priority {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].Priority < sorted[j].Priority
	})
	return sorted
}

func hasActiveWorkers(workers []model.Worker) bool {
	for _, worker := range workers {
		if worker.Active() {
			return true
		}
	}
	return false
}

func evaluateResourceGate(previous model.ResourceGateState, snapshot model.ResourceSnapshot, policy config.Resources, now time.Time) (model.ResourceGateState, bool, string) {
	state := previous
	if snapshot.TotalMemoryBytes == 0 || snapshot.AvailableMemoryBytes > snapshot.TotalMemoryBytes || math.IsNaN(snapshot.CPUUtilizationPercent) || math.IsInf(snapshot.CPUUtilizationPercent, 0) || snapshot.CPUUtilizationPercent < 0 || snapshot.CPUUtilizationPercent > 100 {
		state.Blocked = true
		state.Reason = model.ResourceGateReasonInvalidObservation
		state.HighCPUSince = nil
		state.HealthySince = nil
		return state, false, "resource monitor returned an out-of-range CPU or physical-memory observation"
	}
	if state.Blocked && state.Reason == "" {
		state.HighCPUSince = nil
		state.HealthySince = nil
		if snapshot.CPUUtilizationPercent <= policy.CPUResumePercent {
			return model.ResourceGateState{}, true, ""
		}
		state.Reason = model.ResourceGateReasonCPU
		return state, false, ""
	}
	if !state.Blocked {
		state.Reason = ""
		state.HealthySince = nil
		if snapshot.CPUUtilizationPercent >= policy.CPUBlockPercent {
			if state.HighCPUSince == nil || now.Before(*state.HighCPUSince) {
				value := now
				state.HighCPUSince = &value
			}
			if now.Sub(*state.HighCPUSince) >= policy.CPUObservationWindow.Duration {
				state.Blocked = true
				state.Reason = model.ResourceGateReasonCPU
				state.HealthySince = nil
				return state, false, ""
			}
		} else {
			state.HighCPUSince = nil
		}
		return state, true, ""
	}

	state.HighCPUSince = nil
	if snapshot.CPUUtilizationPercent <= policy.CPUResumePercent {
		if state.HealthySince == nil || now.Before(*state.HealthySince) {
			value := now
			state.HealthySince = &value
		}
		if now.Sub(*state.HealthySince) >= policy.CPUHysteresisWindow.Duration {
			return model.ResourceGateState{}, true, ""
		}
	} else {
		state.HealthySince = nil
	}
	return state, false, ""
}

// memoryBasis is the memory admission input for one plan. With a configured
// WorkerMemoryBudget the slot math runs on remaining = budget minus every
// active worker's reservation: a static basis the workers' own host footprint
// (vmmem) cannot erode, unlike host AvailablePhysical, which falls as workers
// run and re-clamps capacity under exactly the load it should serve. The host
// snapshot then only backs a coarse hard floor that blocks NEW starts and
// advertised growth when the host is genuinely out of memory; the floor sits
// far below any level worker growth alone can reach because the VM's size caps
// the workers' total host footprint.
type memoryBasis struct {
	budgetActive  bool
	budgetClamped bool
	floorBlocked  bool
	remaining     uint64
}

func evaluateMemoryBasis(input PlanInput) memoryBasis {
	budget := uint64(input.Config.Resources.WorkerMemoryBudget)
	if budget == 0 {
		return memoryBasis{remaining: availableMemoryHeadroom(input.Resources, input.Config.Resources)}
	}
	basis := memoryBasis{budgetActive: true}
	if input.EngineMemoryTotalBytes > 0 && budget > input.EngineMemoryTotalBytes {
		budget = input.EngineMemoryTotalBytes
		basis.budgetClamped = true
	}
	reservations := activeWorkerReservations(input.Config, input.Workers)
	if reservations < budget {
		basis.remaining = budget - reservations
	}
	basis.floorBlocked = hostMemoryAtFloor(input.Resources, input.Config.Resources)
	return basis
}

// activeWorkerReservations sums the effective worker memory profile of every
// busy, starting, and idle worker. A static budget seed must subtract busy
// workers explicitly: the legacy host reading excluded them implicitly because
// their consumption had already lowered AvailablePhysical. Workers in unknown
// pools still occupy VM memory, so they reserve the global default profile.
func activeWorkerReservations(cfg config.Config, workers []model.Worker) uint64 {
	memoryByPool := make(map[string]config.ByteSize, len(cfg.GitHub.Targets))
	for _, target := range cfg.GitHub.Targets {
		memoryByPool[target.ID] = target.EffectiveWorker(cfg.Resources.Worker).Memory
	}
	var total uint64
	for _, worker := range workers {
		if !worker.Active() {
			continue
		}
		memory, known := memoryByPool[worker.PoolID]
		if !known {
			memory = cfg.Resources.Worker.Memory
		}
		total = saturatingAddUint64(total, uint64(memory))
	}
	return total
}

// hostMemoryAtFloor reports the budget-basis backstop: host AvailablePhysical
// at or below the MinimumAvailableMemoryPct reserve. Invalid observations are
// not judged here; evaluateResourceGate already fails them closed.
func hostMemoryAtFloor(snapshot model.ResourceSnapshot, policy config.Resources) bool {
	if snapshot.TotalMemoryBytes == 0 || snapshot.AvailableMemoryBytes > snapshot.TotalMemoryBytes {
		return false
	}
	reserved := uint64(math.Ceil(float64(snapshot.TotalMemoryBytes) * policy.MinimumAvailableMemoryPct / 100))
	return snapshot.AvailableMemoryBytes <= reserved
}

func availableMemoryHeadroom(snapshot model.ResourceSnapshot, policy config.Resources) uint64 {
	if snapshot.TotalMemoryBytes == 0 || snapshot.AvailableMemoryBytes > snapshot.TotalMemoryBytes {
		return 0
	}
	reserved := uint64(math.Ceil(float64(snapshot.TotalMemoryBytes) * policy.MinimumAvailableMemoryPct / 100))
	if snapshot.AvailableMemoryBytes <= reserved {
		return 0
	}
	return snapshot.AvailableMemoryBytes - reserved
}

func affordableWorkerCount(memoryAvailable uint64, workerMemory config.ByteSize) int {
	memoryBytes := uint64(workerMemory)
	if memoryBytes == 0 {
		return 0
	}
	slots := memoryAvailable / memoryBytes
	if slots > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(slots)
}

func affordableWorkerCountWithMargin(memoryAvailable uint64, workerMemory config.ByteSize, marginPercent float64) int {
	memoryBytes := uint64(workerMemory)
	if memoryBytes == 0 {
		return 0
	}
	margin := uint64(math.Ceil(float64(memoryBytes) * marginPercent / 100))
	if memoryAvailable <= margin {
		return 0
	}
	return affordableWorkerCount(memoryAvailable-margin, workerMemory)
}

func evaluatePowerGate(previous model.PowerGateState, snapshot model.PowerSnapshot, policy config.Power, now time.Time) (model.PowerGateState, bool) {
	if policy.Policy == config.PowerAlways {
		return model.PowerGateState{}, true
	}
	state := previous
	if !snapshot.ACConnected {
		state.ACSince = nil
		return state, false
	}
	if state.ACSince == nil || now.Before(*state.ACSince) {
		value := now
		state.ACSince = &value
		return state, false
	}
	return state, now.Sub(*state.ACSince) >= policy.StableACWindow.Duration
}

func problem(now time.Time, code, message, poolID string, retryable bool) model.Problem {
	return model.Problem{Code: code, Message: message, PoolID: poolID, Retryable: retryable, At: now}
}
