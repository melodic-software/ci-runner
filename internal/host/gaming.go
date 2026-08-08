package host

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrProbeTimedOut is the cause attached to a probe context that exhausts its
// own budget. Callers use it to tell "the host reported a negative result" from
// "the probe never finished", which the bare context.DeadlineExceeded sentinel
// cannot: one sentinel serves every deadline, including the caller's own.
var ErrProbeTimedOut = errors.New("host probe did not complete within its deadline")

// GamingManager is the one implementation of the host-wide gaming shutdown
// contract. The controller decides when to invoke it; this type owns only the
// concrete Docker Desktop and WSL effects and their postcondition checks.
//
// ProbeTimeout bounds each probe separately rather than the call as a whole. A
// shared budget lets the slowest probe starve the rest: `docker desktop status`
// costs upwards of 16s while Docker Desktop is stopped, which is the state
// gaming mode exists to create, so one aggregate deadline expires inside it and
// every later probe then fails on the spent context without ever running.
//
// DesktopProbeTimeout is separate because dividing one budget evenly still
// cannot fit that probe. `docker desktop status` has no timeout flag of its own
// (its start/stop siblings do), so the budget is the only control, and a probe
// that always expires reports DesktopStatusUnknown rather than the stopped
// state it was asked about. Zero falls back to ProbeTimeout.
type GamingManager struct {
	Desktop             DesktopProcess
	Docker              DockerInspector
	WSL                 WSLManager
	ProbeTimeout        time.Duration
	DesktopProbeTimeout time.Duration
}

// probe derives one probe's deadline from the caller's context. The caller's
// own deadline still bounds the whole, because a derived context expires no
// later than its parent.
func (m GamingManager) probe(ctx context.Context) (context.Context, context.CancelFunc) {
	return budgeted(ctx, m.ProbeTimeout)
}

func (m GamingManager) desktopProbe(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.DesktopProbeTimeout > 0 {
		return budgeted(ctx, m.DesktopProbeTimeout)
	}
	return m.probe(ctx)
}

func budgeted(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeoutCause(ctx, budget, ErrProbeTimedOut)
}

func (m GamingManager) Inventory(ctx context.Context) GamingInventory {
	var inventory GamingInventory

	statusContext, cancelStatus := m.desktopProbe(ctx)
	status, err := m.Desktop.Status(statusContext)
	cancelStatus()
	if err != nil {
		inventory.DesktopStatus = DesktopStatusUnknown
		inventory.Problems = append(inventory.Problems, fmt.Sprintf("Docker Desktop status: %v", err))
	} else {
		inventory.DesktopStatus = status
	}

	engineContext, cancelEngine := m.probe(ctx)
	reachable, err := m.Docker.EngineReachable(engineContext)
	cancelEngine()
	if err != nil {
		inventory.Problems = append(inventory.Problems, fmt.Sprintf("Docker engine status: %v", err))
	} else {
		inventory.DockerReachable = reachable
	}
	if reachable {
		containerContext, cancelContainers := m.probe(ctx)
		containers, containerErr := m.Docker.Containers(containerContext)
		cancelContainers()
		if containerErr != nil {
			inventory.Problems = append(inventory.Problems, fmt.Sprintf("Docker container inventory: %v", containerErr))
		} else {
			for _, container := range containers {
				if container.Managed {
					inventory.CIContainers = append(inventory.CIContainers, container)
				} else {
					inventory.NonCIContainers = append(inventory.NonCIContainers, container)
				}
			}
		}
	}

	wslContext, cancelWSL := m.probe(ctx)
	distributions, err := m.WSL.Running(wslContext)
	cancelWSL()
	if err != nil {
		inventory.Problems = append(inventory.Problems, fmt.Sprintf("WSL inventory: %v", err))
	} else {
		inventory.RunningDistributions = distributions
	}

	return inventory
}

func (m GamingManager) StopAll(ctx context.Context) error {
	// WSL shutdown is attempted even if Docker Desktop reports a stop failure.
	// A partial shutdown is never reported as success; Verify supplies the final
	// authoritative health result.
	var failures []error
	if err := m.Desktop.Stop(ctx); err != nil {
		failures = append(failures, fmt.Errorf("stop Docker Desktop: %w", err))
	}
	if err := m.WSL.Shutdown(ctx); err != nil {
		failures = append(failures, fmt.Errorf("shut down WSL: %w", err))
	}
	return errors.Join(failures...)
}

func (m GamingManager) Verify(ctx context.Context) (GamingVerification, error) {
	var verification GamingVerification
	var failures []error

	statusContext, cancelStatus := m.desktopProbe(ctx)
	status, err := m.Desktop.Status(statusContext)
	cancelStatus()
	if err != nil {
		// Every probe error leaves the postcondition unobserved, not observed
		// false: a launch, parse, or permission failure says as little about the
		// desktop's state as a timeout does.
		verification.DesktopUnverified = true
		failures = append(failures, fmt.Errorf("query Docker Desktop status: %w", err))
	} else {
		verification.DesktopStopped = status == DesktopStatusStopped
		if !verification.DesktopStopped {
			failures = append(failures, fmt.Errorf("unexpected Docker Desktop status %s", status))
		}
	}

	engineContext, cancelEngine := m.probe(ctx)
	reachable, err := m.Docker.EngineReachable(engineContext)
	cancelEngine()
	if err != nil {
		verification.DockerUnverified = true
		failures = append(failures, fmt.Errorf("query Docker engine status: %w", err))
	} else {
		verification.DockerUnreachable = !reachable
		if reachable {
			failures = append(failures, errors.New("docker engine is still reachable"))
		}
	}

	wslContext, cancelWSL := m.probe(ctx)
	distributions, err := m.WSL.Running(wslContext)
	cancelWSL()
	if err != nil {
		verification.WSLUnverified = true
		failures = append(failures, fmt.Errorf("WSL inventory: %w", err))
	} else {
		verification.RunningDistributions = distributions
		verification.NoRunningWSL = len(distributions) == 0
		if !verification.NoRunningWSL {
			failures = append(failures, fmt.Errorf("WSL distributions are still running: %v", distributions))
		}
	}

	return verification, errors.Join(failures...)
}
