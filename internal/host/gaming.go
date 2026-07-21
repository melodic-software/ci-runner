package host

import (
	"context"
	"errors"
	"fmt"
)

// GamingManager is the one implementation of the host-wide gaming shutdown
// contract. The controller decides when to invoke it; this type owns only the
// concrete Docker Desktop and WSL effects and their postcondition checks.
type GamingManager struct {
	Desktop DesktopProcess
	Docker  DockerInspector
	WSL     WSLManager
}

func (m GamingManager) Inventory(ctx context.Context) GamingInventory {
	var inventory GamingInventory

	status, err := m.Desktop.Status(ctx)
	if err != nil {
		inventory.DesktopStatus = DesktopStatusUnknown
		inventory.Problems = append(inventory.Problems, fmt.Sprintf("Docker Desktop status: %v", err))
	} else {
		inventory.DesktopStatus = status
	}

	reachable, err := m.Docker.EngineReachable(ctx)
	if err != nil {
		inventory.Problems = append(inventory.Problems, fmt.Sprintf("Docker engine status: %v", err))
	} else {
		inventory.DockerReachable = reachable
	}
	if reachable {
		containers, containerErr := m.Docker.Containers(ctx)
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

	distributions, err := m.WSL.Running(ctx)
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

	status, err := m.Desktop.Status(ctx)
	if err != nil {
		failures = append(failures, fmt.Errorf("query Docker Desktop status: %w", err))
	} else {
		verification.DesktopStopped = status == DesktopStatusStopped
		if !verification.DesktopStopped {
			failures = append(failures, fmt.Errorf("unexpected Docker Desktop status %s", status))
		}
	}

	reachable, err := m.Docker.EngineReachable(ctx)
	if err != nil {
		failures = append(failures, fmt.Errorf("query Docker engine status: %w", err))
	} else {
		verification.DockerUnreachable = !reachable
		if reachable {
			failures = append(failures, errors.New("docker engine is still reachable"))
		}
	}

	distributions, err := m.WSL.Running(ctx)
	if err != nil {
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
