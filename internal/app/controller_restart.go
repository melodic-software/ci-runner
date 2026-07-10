package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/control"
)

const controllerTaskName = "ci-runner-fleet"

func (a *Application) controllerCommand(ctx context.Context, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(a.errOut, "usage: ci-runner host controller <restart|stop-for-update>")
		return ExitUsage
	}
	switch args[0] {
	case "restart":
		return a.stopController(ctx, true)
	case "stop-for-update":
		return a.stopController(ctx, false)
	default:
		fmt.Fprintln(a.errOut, "usage: ci-runner host controller <restart|stop-for-update>")
		return ExitUsage
	}
}

func (a *Application) stopController(ctx context.Context, restart bool) int {
	if a.dependencies.Control == nil || a.dependencies.Processes == nil || (restart && a.dependencies.Tasks == nil) {
		fmt.Fprintln(a.errOut, "controller lifecycle adapters are unavailable")
		return ExitInvalidConfig
	}

	probeContext, cancelProbe := a.localProbeContext(ctx)
	status, err := a.dependencies.Control.Status(probeContext)
	cancelProbe()
	if errors.Is(err, control.ErrUnavailable) {
		if !restart {
			fmt.Fprintln(a.errOut, "controller is unavailable; a safe update drain cannot be proven")
			return ExitDegraded
		}
		fmt.Fprintln(a.out, "Controller is not running; starting its scheduled task.")
		probeContext, cancelProbe := a.localProbeContext(ctx)
		err := a.dependencies.Tasks.Start(probeContext, controllerTaskName)
		cancelProbe()
		if err != nil {
			fmt.Fprintf(a.errOut, "start controller task: %v\n", err)
			return ExitRuntime
		}
		return a.waitForControllerStart(ctx, buildinfo.Version, 0)
	}
	if err != nil {
		fmt.Fprintf(a.errOut, "query controller: %v\n", err)
		return ExitRuntime
	}
	if status.ProcessID == 0 {
		fmt.Fprintln(a.errOut, "controller returned an invalid process ID")
		return ExitStateChanged
	}
	handle, err := a.dependencies.Processes.Open(status.ProcessID)
	if err != nil {
		fmt.Fprintf(a.errOut, "observe controller process: %v\n", err)
		return ExitRuntime
	}
	defer handle.Close()
	if !status.ShuttingDown {
		reason := "ci-runner host controller stop-for-update"
		if restart {
			reason = "ci-runner host controller restart"
		}
		probeContext, cancelProbe := a.localProbeContext(ctx)
		accepted, err := a.dependencies.Control.Shutdown(probeContext, reason, status, restart)
		cancelProbe()
		if err != nil {
			var responseErr *control.ResponseError
			if errors.As(err, &responseErr) && responseErr.Code == "shutdown-state-changed" {
				fmt.Fprintf(a.errOut, "controller state changed during shutdown preflight: %v\n", err)
				return ExitStateChanged
			}
			fmt.Fprintf(a.errOut, "request clean controller shutdown: %v\n", err)
			return ExitRuntime
		}
		if !accepted.ShuttingDown || accepted.ProcessID != status.ProcessID ||
			accepted.AssignedJobCount != status.AssignedJobCount ||
			accepted.ActiveJobCount != status.ActiveJobCount ||
			accepted.ActiveWorkerCount != status.ActiveWorkerCount {
			fmt.Fprintln(a.errOut, "controller returned an inconsistent shutdown acknowledgement")
			return ExitStateChanged
		}
	}
	fmt.Fprintf(a.out, "Controller accepted a capacity-zero drain (assigned jobs: %d, active jobs: %d, active workers: %d). Existing work will finish naturally; waiting for process exit.\n", status.AssignedJobCount, status.ActiveJobCount, status.ActiveWorkerCount)
	exitCode, err := handle.Wait(ctx)
	if err != nil {
		fmt.Fprintf(a.errOut, "wait for controller exit: %v\n", err)
		return ExitRuntime
	}
	if !restart && exitCode != 0 {
		fmt.Fprintf(a.errOut, "controller exited with code %d; lifecycle mutation is unsafe\n", exitCode)
		return ExitRuntime
	}
	if !restart {
		fmt.Fprintln(a.out, "Controller stopped safely for update without changing desired mode.")
		return ExitOK
	}
	if exitCode == 0 {
		fmt.Fprintln(a.errOut, "controller restart exited successfully, so Task Scheduler recovery will not run")
		return ExitRuntime
	}
	fmt.Fprintln(a.out, "Controller requested Task Scheduler recovery; waiting for the replacement process.")
	return a.waitForControllerStart(ctx, buildinfo.Version, status.ProcessID)
}

func (a *Application) waitForControllerStart(ctx context.Context, expectedVersion string, previousProcessID uint32) int {
	interval := a.dependencies.Config.Controller.ShutdownPollInterval.Duration
	timeout := a.dependencies.Config.Controller.StartupTimeout.Duration
	if interval <= 0 || timeout <= 0 || expectedVersion == "" {
		fmt.Fprintln(a.errOut, "controller shutdownPollInterval, startupTimeout, and expected version must be valid")
		return ExitInvalidConfig
	}
	waitContext, cancelWait := context.WithTimeout(ctx, timeout)
	defer cancelWait()
	for {
		probeContext, cancelProbe := a.localProbeContext(waitContext)
		status, err := a.dependencies.Control.Status(probeContext)
		cancelProbe()
		if err == nil && status.ProcessID != 0 && status.ProcessID != previousProcessID && !status.ShuttingDown {
			if status.Version != expectedVersion {
				fmt.Fprintf(a.errOut, "replacement controller version %q does not match expected version %q\n", status.Version, expectedVersion)
				return ExitStateChanged
			}
			fmt.Fprintf(a.out, "Controller is running (pid %d, version %s, phase %s).\n", status.ProcessID, displayValue(status.Version), status.Phase)
			return ExitOK
		}
		if err != nil && !errors.Is(err, control.ErrUnavailable) {
			fmt.Fprintf(a.errOut, "verify restarted controller: %v\n", err)
			return ExitRuntime
		}
		timer := time.NewTimer(interval)
		select {
		case <-waitContext.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
				fmt.Fprintf(a.errOut, "controller did not start as version %s within %s\n", expectedVersion, timeout)
				return ExitOperationTimedOut
			}
			fmt.Fprintf(a.errOut, "controller restart wait interrupted: %v\n", waitContext.Err())
			return ExitRuntime
		case <-timer.C:
		}
	}
}
