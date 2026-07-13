package controller

import (
	"context"
	"errors"
	"time"

	"github.com/melodic-software/ci-runner/internal/model"
)

type pollCadenceState struct {
	desired           model.DesiredState
	observed          model.ObservedState
	pools             []PoolSnapshot
	workers           []model.Worker
	desktop           model.DesktopStatus
	advertised        map[string]int
	operationProblems []model.Problem
	forcedZero        bool
	checkpointErr     error
}

type pollCadenceResult struct {
	observed      model.ObservedState
	checkpointErr error
}

// pollCheckpoint persists local liveness and gate progress without claiming
// that an in-flight listener capacity change has already been acknowledged.
// This lets a long GitHub poll remain open while host policy is reevaluated on
// the configured reconciliation cadence.
func (r *Reconciler) pollCheckpoint(
	previous model.ObservedState,
	pools []PoolSnapshot,
	workers []model.Worker,
	resources model.ResourceSnapshot,
	power model.PowerSnapshot,
	desktop model.DesktopStatus,
	plan Plan,
	now time.Time,
	operationProblems []model.Problem,
) model.ObservedState {
	previousPools := make(map[string]model.PoolObservation, len(previous.Pools))
	for _, pool := range previous.Pools {
		previousPools[pool.ID] = pool
	}
	observedPools := make([]model.PoolObservation, 0, len(r.config.GitHub.Targets))
	for _, target := range r.config.GitHub.Targets {
		pool := findPool(pools, target.ID)
		prior := previousPools[target.ID]
		acknowledged := prior.CapacityAcknowledged &&
			prior.MaxCapacity == plan.AdvertisedCapacity[target.ID] &&
			prior.ScaleSetID == pool.Identity.ScaleSetID &&
			prior.ListenerID == pool.Identity.ListenerID
		confirmations := 0
		if acknowledged && prior.MaxCapacity == 0 {
			confirmations = prior.ZeroCapacityConfirmations
		}
		updatedAt := r.poolAcknowledgementTransitionAt(
			target.ID, prior, pool.Identity.ScaleSetID, pool.Identity.ListenerID,
			prior.MaxCapacity, plan.AdvertisedCapacity[target.ID], acknowledged, now,
		)
		observedPools = append(observedPools, model.PoolObservation{
			ID: target.ID, ScaleSetID: pool.Identity.ScaleSetID, ListenerID: pool.Identity.ListenerID,
			TotalAssignedJobs: pool.TotalAssignedJobs, MaxCapacity: prior.MaxCapacity, CapacityAcknowledged: acknowledged,
			ZeroCapacityConfirmations: confirmations, DrainServiceCapacity: pool.DrainServiceCapacity,
			DesiredWorkers: plan.DesiredWorkers[target.ID], UpdatedAt: updatedAt,
		})
	}

	phase := plan.Phase
	if len(operationProblems) > 0 {
		phase = model.PhaseDegraded
	}
	var drainStartedAt *time.Time
	if plan.Phase == model.PhaseDraining {
		drainStartedAt = previous.DrainStartedAt
		if drainStartedAt == nil || now.Before(*drainStartedAt) {
			value := now
			drainStartedAt = &value
		}
	}
	problems := append([]model.Problem(nil), plan.Problems...)
	problems = append(problems, operationProblems...)
	return model.ObservedState{
		SchemaVersion: 1, Phase: phase, HeartbeatAt: now, DrainStartedAt: drainStartedAt, Version: r.version,
		Pools: observedPools, Workers: append([]model.Worker(nil), workers...), Resources: resources,
		Power: power, Desktop: desktop, ResourceGate: plan.ResourceGate, PowerGate: plan.PowerGate,
		Problems: problems,
	}
}

func poolAcknowledgementTransitionAt(prior model.PoolObservation, scaleSetID int64, listenerID string, capacity int, acknowledged bool, now time.Time) time.Time {
	if prior.UpdatedAt.IsZero() || prior.ScaleSetID != scaleSetID || prior.ListenerID != listenerID ||
		prior.MaxCapacity != capacity || prior.CapacityAcknowledged != acknowledged {
		return now
	}
	return prior.UpdatedAt
}

func (r *Reconciler) poolAcknowledgementTransitionAt(targetID string, prior model.PoolObservation, scaleSetID int64, listenerID string, capacity, pendingCapacity int, acknowledged bool, now time.Time) time.Time {
	transitionAt := poolAcknowledgementTransitionAt(prior, scaleSetID, listenerID, capacity, acknowledged, now)
	r.capacityMu.Lock()
	defer r.capacityMu.Unlock()
	if r.pendingCapacity == nil {
		r.pendingCapacity = make(map[string]int)
	}
	if acknowledged {
		delete(r.pendingCapacity, targetID)
		return transitionAt
	}
	if previous, found := r.pendingCapacity[targetID]; found && previous != pendingCapacity {
		transitionAt = now
	}
	r.pendingCapacity[targetID] = pendingCapacity
	return transitionAt
}

func (r *Reconciler) watchPollCadence(ctx context.Context, cancel context.CancelCauseFunc, state pollCadenceState) pollCadenceResult {
	result := pollCadenceResult{observed: state.observed, checkpointErr: state.checkpointErr}
	ticker := time.NewTicker(r.config.Controller.ReconcileInterval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return result
		case <-ticker.C:
		}

		now := r.deps.Clock.Now().UTC()
		checkpoint := state.observed
		var observationErr error
		if !state.forcedZero {
			power, powerErr := r.deps.Power.Snapshot(ctx)
			resources, resourceErr := r.deps.Resources.Snapshot(ctx)
			workers, workerErr := r.deps.Workers.List(ctx)
			if workerErr == nil {
				workers, workerErr = r.enrichWorkerJobs(ctx, workers)
			}
			if ctx.Err() != nil {
				return result
			}
			observationErr = errors.Join(powerErr, resourceErr, workerErr)
			if observationErr == nil {
				if !sameWorkerInventory(state.workers, workers) {
					cancel(errReconcileInputsChanged)
					return result
				}
				plan := BuildPlan(PlanInput{
					Config: r.config, Desired: state.desired, Previous: checkpoint, Pools: state.pools,
					Workers: workers, Resources: resources, Power: power, Desktop: state.desktop, Now: now,
				})
				plan.AdvertisedCapacity = sequenceCapacityTransfer(checkpoint, plan.AdvertisedCapacity)
				checkpoint = r.pollCheckpoint(checkpoint, state.pools, state.workers, resources, power, state.desktop, plan, now, state.operationProblems)
				state.observed = checkpoint
				result.observed = checkpoint

				checkpointErr := r.deps.State.SaveObserved(ctx, checkpoint)
				if ctx.Err() == nil {
					result.checkpointErr = checkpointErr
				}
				if !sameCapacities(state.advertised, plan.AdvertisedCapacity) &&
					(checkpointErr == nil || capacityDecreased(state.advertised, plan.AdvertisedCapacity)) {
					cancel(errReconcileInputsChanged)
					return result
				}
				continue
			}
		}

		checkpoint.HeartbeatAt = now
		state.observed = checkpoint
		result.observed = checkpoint
		if checkpointErr := r.deps.State.SaveObserved(ctx, checkpoint); ctx.Err() == nil {
			result.checkpointErr = checkpointErr
		}
		if observationErr != nil {
			cancel(errReconcileInputsChanged)
			return result
		}
	}
}

func containsReadyPool(pools []PoolSnapshot) bool {
	for _, pool := range pools {
		if pool.Ready {
			return true
		}
	}
	return false
}

func sameCapacities(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for poolID, capacity := range left {
		if right[poolID] != capacity {
			return false
		}
	}
	return true
}

func capacityDecreased(previous, current map[string]int) bool {
	for poolID, capacity := range previous {
		if current[poolID] < capacity {
			return true
		}
	}
	return false
}

func sameWorkerInventory(left, right []model.Worker) bool {
	if len(left) != len(right) {
		return false
	}
	workers := make(map[string]model.Worker, len(left))
	for _, worker := range left {
		if worker.ID == "" {
			return false
		}
		if _, duplicate := workers[worker.ID]; duplicate {
			return false
		}
		workers[worker.ID] = worker
	}
	for _, worker := range right {
		prior, found := workers[worker.ID]
		if !found || prior != worker {
			return false
		}
		delete(workers, worker.ID)
	}
	return len(workers) == 0
}
