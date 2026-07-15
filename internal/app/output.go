package app

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/melodic-software/ci-runner/internal/host"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

// CLI output has no safe secondary channel through which to report a write
// failure. Keep the command's established exit-code behavior while making the
// intentional handling of writer errors explicit in one audited location.
func write(w io.Writer, values ...any) {
	_, _ = fmt.Fprint(w, values...)
}

func writef(w io.Writer, format string, values ...any) {
	_, _ = fmt.Fprintf(w, format, values...)
}

func writeln(w io.Writer, values ...any) {
	_, _ = fmt.Fprintln(w, values...)
}

func (a *Application) writeHumanStatus(desired model.DesiredState, desiredErr error, observed model.ObservedState, observedErr error) {
	if errors.Is(desiredErr, state.ErrNotFound) {
		writeln(a.out, "Desired mode: not initialized (safe default: no capacity)")
	} else {
		writef(a.out, "Desired mode: %s\n", desired.Mode)
		if desired.TemporaryCapacityOverride != nil {
			writef(a.out, "Temporary capacity: %d\n", *desired.TemporaryCapacityOverride)
		} else {
			writeln(a.out, "Temporary capacity: configured value")
		}
	}
	if errors.Is(observedErr, state.ErrNotFound) {
		writeln(a.out, "Controller: no observed state yet")
		return
	}
	writef(a.out, "Phase: %s\n", observed.Phase)
	writef(a.out, "Controller version: %s\n", displayValue(observed.Version))
	writef(a.out, "Heartbeat: %s\n", observed.HeartbeatAt.Format("2006-01-02 15:04:05Z07:00"))
	writef(a.out, "Docker Desktop: running=%t engine=%t WSL=%d\n", observed.Desktop.DesktopRunning, observed.Desktop.EngineReachable, observed.Desktop.RunningWSLCount)
	writef(a.out, "Power: AC connected=%t\n", observed.Power.ACConnected)
	memoryPercent := float64(0)
	if observed.Resources.TotalMemoryBytes > 0 {
		memoryPercent = float64(observed.Resources.AvailableMemoryBytes) * 100 / float64(observed.Resources.TotalMemoryBytes)
	}
	writef(a.out, "Resources: CPU %.1f%%, memory available %.1f%%\n", observed.Resources.CPUUtilizationPercent, memoryPercent)

	pools := append([]model.PoolObservation(nil), observed.Pools...)
	sort.Slice(pools, func(i, j int) bool { return pools[i].ID < pools[j].ID })
	if len(pools) > 0 {
		writeln(a.out, "Pools:")
		for _, pool := range pools {
			writef(a.out, "- %s assigned=%d desired=%d advertised-max=%d scale-set=%d\n", pool.ID, pool.TotalAssignedJobs, pool.DesiredWorkers, pool.MaxCapacity, pool.ScaleSetID)
		}
	}
	if len(observed.Workers) > 0 {
		writeln(a.out, "Workers:")
		workers := append([]model.Worker(nil), observed.Workers...)
		sort.Slice(workers, func(i, j int) bool { return workers[i].ID < workers[j].ID })
		for _, worker := range workers {
			writef(a.out, "- %s pool=%s state=%s job=%s\n", worker.Name, worker.PoolID, worker.State, displayValue(worker.JobID))
		}
	}
	if len(observed.Problems) > 0 {
		writeln(a.out, "Problems:")
		for _, problem := range observed.Problems {
			writef(a.out, "- [%s] %s\n", problem.Code, problem.Message)
		}
	}
}

func (a *Application) writeGamingInventory(observed model.ObservedState, inventory host.GamingInventory) {
	writeln(a.out, "Gaming-mode impact inventory:")
	busy := 0
	for _, worker := range observed.Workers {
		if worker.State != model.WorkerBusy {
			continue
		}
		busy++
		writef(a.out, "- Active CI job: worker=%s pool=%s job=%s\n", worker.Name, worker.PoolID, displayValue(worker.JobID))
	}
	if busy == 0 {
		writeln(a.out, "- Active CI jobs: none")
	}
	for _, container := range inventory.NonCIContainers {
		writef(a.out, "- Non-CI Docker container: %s image=%s status=%s\n", container.Name, container.Image, container.Status)
	}
	if len(inventory.NonCIContainers) == 0 {
		writeln(a.out, "- Non-CI Docker containers: none")
	}
	for _, distribution := range inventory.RunningDistributions {
		writef(a.out, "- Running WSL distribution: %s\n", distribution)
	}
	if len(inventory.RunningDistributions) == 0 {
		writeln(a.out, "- Running WSL distributions: none")
	}
	for _, problem := range inventory.Problems {
		writef(a.out, "- Inventory warning: %s\n", problem)
	}
	writeln(a.out, "Docker Desktop and every running WSL distribution will stop after CI jobs drain.")
}
