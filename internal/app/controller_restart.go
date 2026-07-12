package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/state"
)

const (
	controllerTaskName             = "ci-runner-fleet"
	maxControllerTaskStartAttempts = 4
)

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
	if a.dependencies.Control == nil || a.dependencies.Processes == nil ||
		(restart && (a.dependencies.Tasks == nil || a.dependencies.RestartReceipts == nil)) {
		fmt.Fprintln(a.errOut, "controller lifecycle adapters are unavailable")
		return ExitInvalidConfig
	}

	probeContext, cancelProbe := a.localProbeContext(ctx)
	status, err := a.dependencies.Control.Status(probeContext)
	cancelProbe()
	if errors.Is(err, control.ErrUnavailable) {
		fmt.Fprintln(a.errOut, "controller is unavailable; an authenticated capacity-zero drain and exact process exit cannot be proven; scheduled task was not started")
		return ExitDegraded
	}
	if err != nil {
		fmt.Fprintf(a.errOut, "query controller: %v\n", err)
		return ExitRuntime
	}
	if status.ProcessID == 0 {
		fmt.Fprintln(a.errOut, "controller returned an invalid process ID")
		return ExitStateChanged
	}
	if restart && status.Version != buildinfo.Version {
		fmt.Fprintf(a.errOut, "running controller version %q does not match restart command version %q; scheduled task was not started\n", status.Version, buildinfo.Version)
		return ExitStateChanged
	}
	handle, err := a.dependencies.Processes.Open(status.ProcessID)
	if err != nil {
		fmt.Fprintf(a.errOut, "observe controller process: %v\n", err)
		return ExitRuntime
	}
	defer handle.Close()
	restartRequestID := status.RestartRequestID
	if restart && status.ShuttingDown && restartRequestID == "" {
		fmt.Fprintln(a.errOut, "controller is already shutting down without an authenticated restart request ID; scheduled task was not started")
		return ExitStateChanged
	}
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
		if !accepted.ShuttingDown || accepted.ProcessID != status.ProcessID || accepted.Version != status.Version ||
			accepted.AssignedJobCount != status.AssignedJobCount ||
			accepted.ActiveJobCount != status.ActiveJobCount ||
			accepted.ActiveWorkerCount != status.ActiveWorkerCount {
			fmt.Fprintln(a.errOut, "controller returned an inconsistent shutdown acknowledgement")
			return ExitStateChanged
		}
		if restart {
			restartRequestID = accepted.RestartRequestID
			if restartRequestID == "" {
				fmt.Fprintln(a.errOut, "controller restart acknowledgement omitted its authenticated request ID; scheduled task was not started")
				return ExitStateChanged
			}
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
	if exitCode != ControllerRestartExitCode {
		fmt.Fprintf(a.errOut, "controller exited with code %d instead of dedicated restart code %d; scheduled task was not started\n", exitCode, ControllerRestartExitCode)
		return ExitStateChanged
	}
	receiptContext, cancelReceipt := a.localProbeContext(ctx)
	receipt, err := a.dependencies.RestartReceipts.LoadRestartReceipt(receiptContext)
	cancelReceipt()
	if errors.Is(err, state.ErrNotFound) {
		fmt.Fprintln(a.errOut, "controller exited without a durable restart completion receipt; scheduled task was not started")
		return ExitStateChanged
	}
	if err != nil {
		fmt.Fprintf(a.errOut, "read restart completion receipt: %v; scheduled task was not started\n", err)
		return ExitRuntime
	}
	if receipt.SchemaVersion != 1 || receipt.CompletedAt.IsZero() ||
		receipt.RequestID != restartRequestID || receipt.ProcessID != status.ProcessID || receipt.Version != status.Version {
		fmt.Fprintln(a.errOut, "restart completion receipt does not match the authenticated request, old process, and exact version; scheduled task was not started")
		return ExitStateChanged
	}
	fmt.Fprintf(a.out, "Controller exited after the accepted restart drain (code %d); exact durable completion receipt verified.\n", exitCode)
	return a.startControllerTaskAndWait(ctx, buildinfo.Version, status.ProcessID)
}

// startControllerTaskAndWait explicitly invokes the canonical current-user
// task at most four times, with exponential backoff, until the authenticated
// control plane proves that a different process is running at the exact
// expected version. Task Scheduler can still be finishing the prior instance
// when ProcessHandle.Wait returns; with MultipleInstances=IgnoreNew an
// otherwise successful /Run request is then a no-op. Bounded retries close that
// race without creating a task-start storm, starting the controller directly,
// elevating, or terminating either controller process.
func (a *Application) startControllerTaskAndWait(ctx context.Context, expectedVersion string, previousProcessID uint32) int {
	interval := a.dependencies.Config.Controller.ShutdownPollInterval.Duration
	timeout := a.dependencies.Config.Controller.StartupTimeout.Duration
	if a.dependencies.Control == nil || a.dependencies.Tasks == nil || interval <= 0 || timeout <= 0 || expectedVersion == "" || previousProcessID == 0 {
		fmt.Fprintln(a.errOut, "controller adapters, shutdownPollInterval, startupTimeout, expected version, and previous process ID must be valid")
		return ExitInvalidConfig
	}
	waitContext, cancelWait := context.WithTimeout(ctx, timeout)
	defer cancelWait()
	fmt.Fprintf(a.out, "Starting canonical scheduled task %q and waiting for a verified replacement process.\n", controllerTaskName)
	var lastStartErr error
	startAttempts := 0
	var nextStartAt time.Time
	for {
		probeContext, cancelProbe := a.localProbeContext(waitContext)
		status, err := a.dependencies.Control.Status(probeContext)
		cancelProbe()
		if err == nil {
			if status.ProcessID == 0 {
				fmt.Fprintln(a.errOut, "replacement controller returned an invalid process ID")
				return ExitStateChanged
			}
			if status.ProcessID == previousProcessID {
				fmt.Fprintf(a.errOut, "controller control plane still reports exited process ID %d; scheduled task was not started again\n", previousProcessID)
				return ExitStateChanged
			}
			if status.Version != expectedVersion {
				fmt.Fprintf(a.errOut, "replacement controller version %q does not match expected version %q\n", status.Version, expectedVersion)
				return ExitStateChanged
			}
			if !status.ShuttingDown {
				fmt.Fprintf(a.out, "Controller is running (pid %d, version %s, phase %s).\n", status.ProcessID, displayValue(status.Version), status.Phase)
				return ExitOK
			}
		}
		if err != nil && !errors.Is(err, control.ErrUnavailable) {
			fmt.Fprintf(a.errOut, "verify restarted controller: %v\n", err)
			return ExitRuntime
		}

		now := time.Now()
		if startAttempts < maxControllerTaskStartAttempts && (nextStartAt.IsZero() || !now.Before(nextStartAt)) {
			startContext, cancelStart := a.localProbeContext(waitContext)
			startErr := a.dependencies.Tasks.Start(startContext, controllerTaskName)
			cancelStart()
			startAttempts++
			lastStartErr = startErr
			multiplier := time.Duration(1 << uint(startAttempts-1))
			backoff := timeout
			if interval <= timeout/multiplier {
				backoff = interval * multiplier
			}
			nextStartAt = now.Add(backoff)
		}
		timer := time.NewTimer(interval)
		select {
		case <-waitContext.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
				if lastStartErr != nil {
					fmt.Fprintf(a.errOut, "last canonical task start attempt failed: %v\n", lastStartErr)
				}
				fmt.Fprintf(a.errOut, "controller did not start as version %s within %s after %d of at most %d canonical task start attempt(s)\n", expectedVersion, timeout, startAttempts, maxControllerTaskStartAttempts)
				return ExitOperationTimedOut
			}
			fmt.Fprintf(a.errOut, "controller restart wait interrupted: %v\n", waitContext.Err())
			return ExitRuntime
		case <-timer.C:
		}
	}
}
