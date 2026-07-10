package app

import (
	"errors"
	"fmt"
	"sort"

	"github.com/melodic-software/ci-runner/internal/host"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

func (a *Application) writeHumanStatus(desired model.DesiredState, desiredErr error, observed model.ObservedState, observedErr error) {
	if errors.Is(desiredErr, state.ErrNotFound) {
		fmt.Fprintln(a.out, "Desired mode: not initialized (safe default: no capacity)")
	} else {
		fmt.Fprintf(a.out, "Desired mode: %s\n", desired.Mode)
		if desired.TemporaryCapacityOverride != nil {
			fmt.Fprintf(a.out, "Temporary capacity: %d\n", *desired.TemporaryCapacityOverride)
		} else {
			fmt.Fprintln(a.out, "Temporary capacity: configured value")
		}
	}
	if errors.Is(observedErr, state.ErrNotFound) {
		fmt.Fprintln(a.out, "Controller: no observed state yet")
		return
	}
	fmt.Fprintf(a.out, "Phase: %s\n", observed.Phase)
	fmt.Fprintf(a.out, "Controller version: %s\n", displayValue(observed.Version))
	fmt.Fprintf(a.out, "Heartbeat: %s\n", observed.HeartbeatAt.Format("2006-01-02 15:04:05Z07:00"))
	fmt.Fprintf(a.out, "Docker Desktop: running=%t engine=%t WSL=%d\n", observed.Desktop.DesktopRunning, observed.Desktop.EngineReachable, observed.Desktop.RunningWSLCount)
	fmt.Fprintf(a.out, "Power: AC connected=%t\n", observed.Power.ACConnected)
	memoryPercent := float64(0)
	if observed.Resources.TotalMemoryBytes > 0 {
		memoryPercent = float64(observed.Resources.AvailableMemoryBytes) * 100 / float64(observed.Resources.TotalMemoryBytes)
	}
	fmt.Fprintf(a.out, "Resources: CPU %.1f%%, memory available %.1f%%\n", observed.Resources.CPUUtilizationPercent, memoryPercent)

	pools := append([]model.PoolObservation(nil), observed.Pools...)
	sort.Slice(pools, func(i, j int) bool { return pools[i].ID < pools[j].ID })
	if len(pools) > 0 {
		fmt.Fprintln(a.out, "Pools:")
		for _, pool := range pools {
			fmt.Fprintf(a.out, "- %s assigned=%d desired=%d advertised-max=%d scale-set=%d\n", pool.ID, pool.TotalAssignedJobs, pool.DesiredWorkers, pool.MaxCapacity, pool.ScaleSetID)
		}
	}
	if len(observed.Workers) > 0 {
		fmt.Fprintln(a.out, "Workers:")
		workers := append([]model.Worker(nil), observed.Workers...)
		sort.Slice(workers, func(i, j int) bool { return workers[i].ID < workers[j].ID })
		for _, worker := range workers {
			fmt.Fprintf(a.out, "- %s pool=%s state=%s job=%s\n", worker.Name, worker.PoolID, worker.State, displayValue(worker.JobID))
		}
	}
	if len(observed.Problems) > 0 {
		fmt.Fprintln(a.out, "Problems:")
		for _, problem := range observed.Problems {
			fmt.Fprintf(a.out, "- [%s] %s\n", problem.Code, problem.Message)
		}
	}
}

func (a *Application) writeGamingInventory(observed model.ObservedState, inventory host.GamingInventory) {
	fmt.Fprintln(a.out, "Gaming-mode impact inventory:")
	busy := 0
	for _, worker := range observed.Workers {
		if worker.State != model.WorkerBusy {
			continue
		}
		busy++
		fmt.Fprintf(a.out, "- Active CI job: worker=%s pool=%s job=%s\n", worker.Name, worker.PoolID, displayValue(worker.JobID))
	}
	if busy == 0 {
		fmt.Fprintln(a.out, "- Active CI jobs: none")
	}
	for _, container := range inventory.NonCIContainers {
		fmt.Fprintf(a.out, "- Non-CI Docker container: %s image=%s status=%s\n", container.Name, container.Image, container.Status)
	}
	if len(inventory.NonCIContainers) == 0 {
		fmt.Fprintln(a.out, "- Non-CI Docker containers: none")
	}
	for _, distribution := range inventory.RunningDistributions {
		fmt.Fprintf(a.out, "- Running WSL distribution: %s\n", distribution)
	}
	if len(inventory.RunningDistributions) == 0 {
		fmt.Fprintln(a.out, "- Running WSL distributions: none")
	}
	for _, problem := range inventory.Problems {
		fmt.Fprintf(a.out, "- Inventory warning: %s\n", problem)
	}
	fmt.Fprintln(a.out, "Docker Desktop and every running WSL distribution will stop after CI jobs drain.")
}
