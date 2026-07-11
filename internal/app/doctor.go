package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

type DoctorCheck struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail"`
}

func (a *Application) doctor(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("host doctor", flag.ContinueOnError)
	flags.SetOutput(a.errOut)
	jsonOutput := flags.Bool("json", false, "write machine-readable checks")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	checks := []DoctorCheck{{Name: "configuration", Healthy: true, Detail: "strict configuration loaded and validated"}}

	desired, desiredErr := a.dependencies.Store.LoadDesired(ctx)
	desiredValid := false
	switch {
	case errors.Is(desiredErr, state.ErrNotFound):
		checks = append(checks, DoctorCheck{Name: "desired-state", Healthy: false, Detail: "not initialized; run host enable, disable, or game"})
	case desiredErr != nil:
		checks = append(checks, DoctorCheck{Name: "desired-state", Healthy: false, Detail: desiredErr.Error()})
	default:
		desiredValid = desired.Mode.Valid()
		checks = append(checks, DoctorCheck{Name: "desired-state", Healthy: desiredValid, Detail: string(desired.Mode)})
	}

	var liveStatus *control.Status
	if a.dependencies.Control == nil {
		checks = append(checks, DoctorCheck{Name: "controller-control-plane", Healthy: false, Detail: "controller control-plane dependency is unavailable"})
	} else {
		probeContext, cancelProbe := a.localProbeContext(ctx)
		status, err := a.dependencies.Control.Status(probeContext)
		cancelProbe()
		if err != nil {
			checks = append(checks, DoctorCheck{Name: "controller-control-plane", Healthy: false, Detail: err.Error()})
		} else {
			liveStatus = &status
			healthy := status.ProcessID != 0 && validPhase(status.Phase) && status.Phase != model.PhaseDegraded && strings.TrimSpace(status.Version) != "" && !status.ShuttingDown
			detail := fmt.Sprintf("pid=%d phase=%s version=%s activeJobs=%d shuttingDown=%t", status.ProcessID, status.Phase, displayValue(status.Version), status.ActiveJobCount, status.ShuttingDown)
			checks = append(checks, DoctorCheck{Name: "controller-control-plane", Healthy: healthy, Detail: detail})
		}
	}

	observed, observedErr := a.dependencies.Store.LoadObserved(ctx)
	switch {
	case errors.Is(observedErr, state.ErrNotFound):
		checks = append(checks, DoctorCheck{Name: "observed-state", Healthy: false, Detail: "no observed state; the scheduled controller may not have started"})
	case observedErr != nil:
		checks = append(checks, DoctorCheck{Name: "observed-state", Healthy: false, Detail: observedErr.Error()})
	default:
		now := a.dependencies.Now().UTC()
		maximumAge := observedFreshnessLimit(a.dependencies.Config)
		age := now.Sub(observed.HeartbeatAt)
		futureTolerance := 2 * a.dependencies.Config.Controller.ReconcileInterval.Duration
		fresh := !observed.HeartbeatAt.IsZero() && age <= maximumAge && age >= -futureTolerance
		healthy := observed.SchemaVersion == 1 && validPhase(observed.Phase) && observed.Phase != model.PhaseDegraded && fresh
		if liveStatus != nil && observed.Version != liveStatus.Version {
			healthy = false
		}
		detail := fmt.Sprintf("phase=%s version=%s heartbeat=%s age=%s maximumAge=%s", observed.Phase, displayValue(observed.Version), observed.HeartbeatAt.Format(time.RFC3339), age.Round(time.Second), maximumAge)
		checks = append(checks, DoctorCheck{Name: "observed-state", Healthy: healthy, Detail: detail})
		pools := make(map[string]model.PoolObservation, len(observed.Pools))
		for _, pool := range observed.Pools {
			pools[pool.ID] = pool
		}
		for _, target := range a.dependencies.Config.GitHub.Targets {
			pool, found := pools[target.ID]
			listenerHealthy := found && pool.ScaleSetID > 0 && pool.ListenerID != "" && pool.CapacityAcknowledged
			listenerDetail := "no current listener observation"
			if found {
				listenerDetail = fmt.Sprintf("scaleSetId=%d listenerId=%s capacity=%d assigned=%d acknowledged=%t", pool.ScaleSetID, displayValue(pool.ListenerID), pool.MaxCapacity, pool.TotalAssignedJobs, pool.CapacityAcknowledged)
			}
			checks = append(checks, DoctorCheck{Name: "github-listener/" + target.ID, Healthy: listenerHealthy, Detail: listenerDetail})
		}
		for _, problem := range observed.Problems {
			checks = append(checks, DoctorCheck{Name: "problem/" + problem.Code, Healthy: false, Detail: problem.Message})
		}
	}

	dockerReachable := false
	if a.dependencies.Gaming == nil {
		checks = append(checks, DoctorCheck{Name: "host-inventory", Healthy: false, Detail: "host inventory dependency is unavailable"})
	} else {
		probeContext, cancelProbe := a.localProbeContext(ctx)
		inventory := a.dependencies.Gaming.Inventory(probeContext)
		cancelProbe()
		dockerReachable = inventory.DockerReachable
		checks = append(checks, DoctorCheck{Name: "docker-desktop-cli", Healthy: inventory.DesktopStatus != "unknown", Detail: string(inventory.DesktopStatus)})
		for index, problem := range inventory.Problems {
			checks = append(checks, DoctorCheck{Name: fmt.Sprintf("host-inventory/%d", index+1), Healthy: false, Detail: problem})
		}
		if desiredValid && desired.Mode == model.ModeGaming {
			probeContext, cancelProbe := a.localProbeContext(ctx)
			verification, err := a.dependencies.Gaming.Verify(probeContext)
			cancelProbe()
			detail := fmt.Sprintf("desktopStopped=%t dockerUnreachable=%t noRunningWSL=%t", verification.DesktopStopped, verification.DockerUnreachable, verification.NoRunningWSL)
			if err != nil {
				detail += ": " + err.Error()
			}
			checks = append(checks, DoctorCheck{Name: "gaming-postconditions", Healthy: err == nil && verification.DesktopStopped && verification.DockerUnreachable && verification.NoRunningWSL, Detail: detail})
		}
	}

	requireDocker := desiredValid && desired.Mode == model.ModeEnabled
	if liveStatus != nil && liveStatus.Phase == model.PhasePowerSuspended {
		requireDocker = false
	}
	if a.dependencies.Doctor == nil {
		checks = append(checks, DoctorCheck{Name: "host-security-and-runtime", Healthy: false, Detail: "doctor inspector dependency is unavailable"})
	} else {
		probeContext, cancelProbe := a.localProbeContext(ctx)
		checks = append(checks, a.dependencies.Doctor.Inspect(probeContext, DoctorInspection{
			CheckDocker:   dockerReachable,
			RequireDocker: requireDocker,
		})...)
		cancelProbe()
	}

	if *jsonOutput {
		encoder := json.NewEncoder(a.out)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(struct {
			Checks []DoctorCheck `json:"checks"`
		}{Checks: checks}); err != nil {
			fmt.Fprintf(a.errOut, "write doctor output: %v\n", err)
			return ExitRuntime
		}
	} else {
		for _, check := range checks {
			status := "PASS"
			if check.Skipped {
				status = "SKIP"
			} else if !check.Healthy {
				status = "FAIL"
			}
			fmt.Fprintf(a.out, "[%s] %s: %s\n", status, check.Name, check.Detail)
		}
	}
	for _, check := range checks {
		if !check.Skipped && !check.Healthy {
			return ExitDegraded
		}
	}
	return ExitOK
}

func validPhase(phase model.Phase) bool {
	switch phase {
	case model.PhaseStarting, model.PhaseReady, model.PhaseResourceConstrained, model.PhasePowerSuspended,
		model.PhaseDraining, model.PhaseDisabled, model.PhaseGaming, model.PhaseDegraded:
		return true
	default:
		return false
	}
}

func observedFreshnessLimit(cfg config.Config) time.Duration {
	return saturatingFreshnessDuration(
		cfg.GitHub.RequestTimeout.Duration,
		cfg.GitHub.Retry.Maximum.Duration,
		cfg.Controller.ReconcileInterval.Duration,
		cfg.GitHub.Retry.MaxAttempts,
		len(cfg.GitHub.Targets),
	)
}

func saturatingFreshnessDuration(request, retryMaximum, reconcile time.Duration, attempts, targets int) time.Duration {
	const maximum = time.Duration(1<<63 - 1)
	if attempts < 1 {
		attempts = 1
	}
	if targets < 1 {
		targets = 1
	}
	perAttempt := request
	if retryMaximum > maximum-perAttempt {
		return maximum
	}
	perAttempt += retryMaximum
	if perAttempt > maximum/time.Duration(attempts) {
		return maximum
	}
	result := perAttempt * time.Duration(attempts)
	if result > maximum/time.Duration(targets) {
		return maximum
	}
	result *= time.Duration(targets)
	if reconcile > maximum/2 || result > maximum-2*reconcile {
		return maximum
	}
	return result + 2*reconcile
}
