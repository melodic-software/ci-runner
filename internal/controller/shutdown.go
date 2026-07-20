package controller

import (
	"context"
	"errors"

	"github.com/melodic-software/ci-runner/internal/clock"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/model"
)

// BeginShutdown enables a transient capacity-zero drain. It never overwrites
// the user's desired.json state.
func (r *Reconciler) BeginShutdown() {
	r.stateMu.Lock()
	r.shuttingDown = true
	cancel := r.currentStepCancel
	r.stateMu.Unlock()
	if cancel != nil {
		// Interrupt an in-flight long poll immediately. The app-level loop also
		// cancels its step context after the acceptance response is flushed, but
		// BeginShutdown cannot depend on scheduling order for responsiveness.
		cancel(errShutdownRequested)
	}
}

func (r *Reconciler) ShuttingDown() bool {
	return r.isShuttingDown()
}

func (r *Reconciler) isShuttingDown() bool {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.shuttingDown
}

// shutdownDegradedStepBudget bounds how many consecutive erroring drain Steps
// Shutdown tolerates before terminating. A clean drain is verified only through
// Step, but persistently failing probes (for example after Docker Desktop was
// intentionally stopped, or an external dependency is down) can make Step return
// an error forever. Rather than loop until the process is externally killed, the
// controller terminates the drain after this many consecutive failures. A clean
// Step resets the count, so transient blips never trip it.
const shutdownDegradedStepBudget = 3

// ErrShutdownDegraded reports that Shutdown terminated through the bounded
// degraded escape: the drain was never verified as complete because Step kept
// erroring. Restart handling treats it as completable -- the scheduled task
// starts a fresh controller either way -- while every other shutdown flavor
// (disable, stop-for-update, process interrupt) must fail closed rather than
// report an unverified drain as a safe stop.
var ErrShutdownDegraded = errors.New("shutdown drain degraded: consecutive reconcile steps failed before the drain was verified")

// Shutdown advertises zero capacity, conditionally removes idle workers, waits
// for busy work to finish naturally, then closes message sessions and the
// worker runtime. Cancellation leaves active work intact and never implies a
// force stop. A Step that succeeds but is not yet drained keeps waiting
// indefinitely for active work; only persistent Step errors are bounded.
func (r *Reconciler) Shutdown(ctx context.Context) error {
	r.BeginShutdown()
	degradedSteps := 0
	degraded := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := r.Step(ctx)
		if err == nil {
			degradedSteps = 0
			if shutdownDrained(result.Observed, r.config.GitHub.Targets) {
				break
			}
		} else {
			degradedSteps++
			if degradedSteps >= shutdownDegradedStepBudget {
				degraded = true
				break
			}
		}
		if err := clock.Sleep(ctx, r.config.Controller.ShutdownPollInterval.Duration); err != nil {
			return err
		}
	}

	var closeErrors []error
	if degraded {
		closeErrors = append(closeErrors, ErrShutdownDegraded)
	}
	if closer, ok := r.deps.ScaleSets.(interface{ Close(context.Context) error }); ok {
		closeErrors = append(closeErrors, closer.Close(ctx))
	}
	if closer, ok := r.deps.Workers.(interface{ Close() error }); ok {
		closeErrors = append(closeErrors, closer.Close())
	}
	return errors.Join(closeErrors...)
}

func shutdownDrained(observed model.ObservedState, targets []config.Target) bool {
	if len(observed.Pools) != len(targets) {
		return false
	}
	expected := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		expected[target.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(observed.Pools))
	for _, pool := range observed.Pools {
		if _, configured := expected[pool.ID]; !configured {
			return false
		}
		if _, duplicate := seen[pool.ID]; duplicate {
			return false
		}
		seen[pool.ID] = struct{}{}
		if !pool.CapacityAcknowledged || pool.MaxCapacity != 0 || pool.TotalAssignedJobs != 0 || pool.DrainServiceCapacity != 0 || pool.ZeroCapacityConfirmations < 2 {
			return false
		}
	}
	for _, worker := range observed.Workers {
		if worker.Active() {
			return false
		}
	}
	return true
}
