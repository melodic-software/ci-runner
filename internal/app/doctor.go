package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/controller"
	"github.com/melodic-software/ci-runner/internal/host"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

type DoctorCheck struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Skipped bool   `json:"skipped,omitempty"`
	// Advisory marks a check whose unhealthy result is expected operational
	// state to surface, not a fault: it renders as WARN and never degrades the
	// doctor exit code.
	Advisory bool   `json:"advisory,omitempty"`
	Detail   string `json:"detail"`
}

func desiredStateDetail(mode model.Mode) string {
	switch mode {
	case model.ModeDisabled:
		return "disabled; persisted in desired.json across reboot; ci-runner host enable is required to resume runners"
	case model.ModeGaming:
		return "gaming; persisted in desired.json across reboot"
	default:
		return string(mode)
	}
}

func (a *Application) doctor(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("host doctor", flag.ContinueOnError)
	flags.SetOutput(a.errOut)
	jsonOutput := flags.Bool("json", false, "write machine-readable checks")
	includeElevated := flags.Bool("include-elevated", false, "include the BitLocker probe, which may open an Administrator UAC prompt")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	if *includeElevated {
		writeln(a.errOut, "WARNING: --include-elevated may open an Administrator UAC prompt while verifying BitLocker; continue only when that prompt is expected.")
	}
	checks := []DoctorCheck{{Name: "configuration", Healthy: true, Detail: "strict configuration loaded and validated"}}

	desired, desiredErr := a.dependencies.Store.LoadDesired(ctx)
	desiredValid := false
	switch {
	case errors.Is(desiredErr, state.ErrNotFound):
		checks = append(checks, DoctorCheck{Name: "desired-state", Healthy: false, Detail: "not initialized; run host enable, disable, or game; a reboot does not enable the host"})
	case desiredErr != nil:
		checks = append(checks, DoctorCheck{Name: "desired-state", Healthy: false, Detail: desiredErr.Error()})
	default:
		desiredValid = desired.Mode.Valid()
		checks = append(checks, DoctorCheck{Name: "desired-state", Healthy: desiredValid, Detail: desiredStateDetail(desired.Mode)})
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
		// A live control plane over a stale heartbeat is the #331 wedge: the
		// process answers while its reconcile loop has stopped.
		if liveStatus != nil && !liveStatus.ShuttingDown {
			livenessLimit := controller.ReconcileLivenessLimit(a.dependencies.Config.Controller.ReconcileInterval.Duration)
			checks = append(checks, DoctorCheck{
				Name:    "controller-reconcile-liveness",
				Healthy: !observed.HeartbeatAt.IsZero() && age <= livenessLimit,
				Detail:  fmt.Sprintf("heartbeatAge=%s maximumAge=%s", age.Round(time.Second), livenessLimit),
			})
		}
		// desired=enabled while observed is still disabled is the #277 never-ready wedge; PhaseStarting
		// is normal until the configured startup budget expires.
		if desiredValid && desired.Mode == model.ModeEnabled && liveStatus != nil && !liveStatus.ShuttingDown {
			startupGrace := a.dependencies.Config.Controller.StartupTimeout.Duration
			neverReady := observed.Phase == model.PhaseDisabled
			startupWedged := observed.Phase == model.PhaseStarting && startupGrace > 0 &&
				!desired.UpdatedAt.IsZero() && now.Sub(desired.UpdatedAt) > startupGrace
			if neverReady || startupWedged {
				checks = append(checks, DoctorCheck{
					Name:    "controller-reconcile-progress",
					Healthy: false,
					Detail:  fmt.Sprintf("desired=%s observedPhase=%s; live controller has not reached a serving phase", desired.Mode, observed.Phase),
				})
			}
		}
		pools := make(map[string]model.PoolObservation, len(observed.Pools))
		for _, pool := range observed.Pools {
			pools[pool.ID] = pool
		}
		for _, target := range a.dependencies.Config.GitHub.Targets {
			pool, found := pools[target.ID]
			acknowledgementAge := now.Sub(pool.UpdatedAt)
			acknowledgementGrace := listenerAcknowledgementGrace(a.dependencies.Config)
			acknowledgementPendingWithinGrace := found && !pool.CapacityAcknowledged && !pool.UpdatedAt.IsZero() &&
				acknowledgementAge >= 0 && acknowledgementAge <= acknowledgementGrace
			// Acknowledged zero while the planner wants workers is the #281 handshake wedge; planned
			// quiesce/drain also advertises zero, so only a serving-ready pool counts.
			acknowledgedZeroStarved := desiredValid && desired.Mode == model.ModeEnabled &&
				observed.Phase == model.PhaseReady &&
				pool.CapacityAcknowledged && pool.DesiredWorkers > 0 && pool.MaxCapacity == 0
			listenerHealthy := found && pool.ScaleSetID > 0 && pool.ListenerID != "" &&
				(pool.CapacityAcknowledged || acknowledgementPendingWithinGrace) &&
				!acknowledgedZeroStarved
			listenerDetail := "no current listener observation"
			if found {
				listenerDetail = fmt.Sprintf("scaleSetId=%d listenerId=%s desiredWorkers=%d capacity=%d assigned=%d acknowledged=%t transitionAge=%s grace=%s", pool.ScaleSetID, displayValue(pool.ListenerID), pool.DesiredWorkers, pool.MaxCapacity, pool.TotalAssignedJobs, pool.CapacityAcknowledged, acknowledgementAge.Round(time.Second), acknowledgementGrace)
			}
			checks = append(checks, DoctorCheck{Name: "github-listener/" + target.ID, Healthy: listenerHealthy, Detail: listenerDetail})
		}
		for _, problem := range observed.Problems {
			checks = append(checks, DoctorCheck{Name: "problem/" + problem.Code, Healthy: false, Detail: problem.Message})
		}
		// resource-constrained with every pool at zero is total starvation —
		// the #281 recurrence that sat all-PASS while the fleet served nothing.
		if desiredValid && desired.Mode == model.ModeEnabled && observed.Phase == model.PhaseResourceConstrained {
			starved := true
			for _, pool := range observed.Pools {
				if pool.MaxCapacity > 0 || pool.TotalAssignedJobs > 0 {
					starved = false
					break
				}
			}
			for _, worker := range observed.Workers {
				if worker.State == model.WorkerBusy || worker.State == model.WorkerStarting {
					starved = false
					break
				}
			}
			if starved {
				checks = append(checks, DoctorCheck{
					Name:    "capacity-starved",
					Healthy: false,
					Detail:  fmt.Sprintf("phase=%s; every pool advertised capacity 0 and no work is active", observed.Phase),
				})
			}
		}
	}

	dockerReachable := false
	if a.dependencies.Gaming == nil {
		checks = append(checks, DoctorCheck{Name: "host-inventory", Healthy: false, Detail: "host inventory dependency is unavailable"})
	} else {
		// Gaming probes carry their own deadlines; an aggregate budget here would starve them.
		gamingMode := desiredValid && desired.Mode == model.ModeGaming
		inventory := a.dependencies.Gaming.Inventory(ctx)
		dockerReachable = inventory.DockerReachable
		// A stopped Docker Desktop is the state gaming mode is built to produce,
		// so surfacing it is right but failing on it is not.
		checks = append(checks, DoctorCheck{
			Name:     "docker-desktop-cli",
			Healthy:  inventory.DesktopStatus != "unknown",
			Advisory: gamingMode,
			Detail:   string(inventory.DesktopStatus),
		})
		for index, problem := range inventory.Problems {
			checks = append(checks, DoctorCheck{Name: fmt.Sprintf("host-inventory/%d", index+1), Healthy: false, Advisory: gamingMode, Detail: problem})
		}
		if gamingMode {
			verification, err := a.dependencies.Gaming.Verify(ctx)
			detail := fmt.Sprintf("desktopStopped=%s dockerUnreachable=%s noRunningWSL=%s",
				postconditionState(verification.DesktopStopped, verification.DesktopUnverified),
				postconditionState(verification.DockerUnreachable, verification.DockerUnverified),
				postconditionState(verification.NoRunningWSL, verification.WSLUnverified))
			if err != nil {
				detail += ": " + err.Error()
			}
			// Unverified postconditions are observation gaps, so they WARN; an observed violation still fails.
			unverified := verification.DesktopUnverified || verification.DockerUnverified || verification.WSLUnverified
			checks = append(checks, DoctorCheck{
				Name:     "gaming-postconditions",
				Healthy:  err == nil && verification.DesktopStopped && verification.DockerUnreachable && verification.NoRunningWSL,
				Advisory: unverified && !observedGamingViolation(verification),
				Detail:   detail,
			})
		}
	}

	requireDocker := desiredValid && desired.Mode == model.ModeEnabled
	if liveStatus != nil && liveStatus.Phase == model.PhasePowerSuspended {
		requireDocker = false
	}
	if a.dependencies.Doctor == nil {
		checks = append(checks, DoctorCheck{Name: "host-security-and-runtime", Healthy: false, Detail: "doctor inspector dependency is unavailable"})
	} else {
		// Pass the command context through: an aggregate budget would starve the per-probe deadlines
		// and cap the elevated probe below human speed.
		checks = append(checks, a.dependencies.Doctor.Inspect(ctx, DoctorInspection{
			CheckDocker:     dockerReachable,
			RequireDocker:   requireDocker,
			IncludeElevated: *includeElevated,
		})...)
	}

	if *jsonOutput {
		if code := a.writeIndentedJSON(struct {
			Checks []DoctorCheck `json:"checks"`
		}{Checks: checks}, "doctor output"); code != ExitOK {
			return code
		}
	} else {
		for _, check := range checks {
			status := "PASS"
			switch {
			case check.Skipped:
				status = "SKIP"
			case !check.Healthy && check.Advisory:
				status = "WARN"
			case !check.Healthy:
				status = "FAIL"
			}
			writef(a.out, "[%s] %s: %s\n", status, check.Name, check.Detail)
		}
	}
	for _, check := range checks {
		if !check.Skipped && !check.Advisory && !check.Healthy {
			return ExitDegraded
		}
	}
	return ExitOK
}

// listenerAcknowledgementConvergenceLegs counts the request paths an acknowledgement crosses:
// the controller advertising capacity, then a later reconcile reading it back.
const listenerAcknowledgementConvergenceLegs = 2

// listenerAcknowledgementGrace budgets a full retry envelope per convergence leg: the heartbeat
// stays fresh while a poll retries, so a shorter window hard-faults a healthy listener.
func listenerAcknowledgementGrace(cfg config.Config) time.Duration {
	return saturatingFreshnessDuration(
		cfg.GitHub.RequestTimeout.Duration,
		maxJitteredBackoff(cfg),
		cfg.Controller.ReconcileInterval.Duration,
		saturatingMulInt(
			max(cfg.GitHub.Retry.MaxAttempts, 1),
			listenerAcknowledgementConvergenceLegs,
		),
	)
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

// observedFreshnessLimit bounds heartbeat staleness on a healthy host: one retry envelope per
// target plus two reconcile intervals.
func observedFreshnessLimit(cfg config.Config) time.Duration {
	return saturatingFreshnessDuration(
		cfg.GitHub.RequestTimeout.Duration,
		cfg.GitHub.Retry.Maximum.Duration,
		cfg.Controller.ReconcileInterval.Duration,
		saturatingMulInt(
			max(cfg.GitHub.Retry.MaxAttempts, 1),
			max(len(cfg.GitHub.Targets), 1),
		),
	)
}

// saturatingFreshnessDuration returns retryUnits*(request+retryBackoff) + 2*reconcile, saturating.
func saturatingFreshnessDuration(request, retryBackoff, reconcile time.Duration, retryUnits int) time.Duration {
	const maximum = time.Duration(1<<63 - 1)
	retryUnits = max(retryUnits, 1)
	perAttempt := request
	if retryBackoff > maximum-perAttempt {
		return maximum
	}
	perAttempt += retryBackoff
	if perAttempt > maximum/time.Duration(retryUnits) {
		return maximum
	}
	result := perAttempt * time.Duration(retryUnits)
	if reconcile > maximum/2 || result > maximum-2*reconcile {
		return maximum
	}
	return result + 2*reconcile
}

// observedGamingViolation reports whether any postcondition was actually
// checked and found unsatisfied, as opposed to merely unverified.
func observedGamingViolation(verification host.GamingVerification) bool {
	return (!verification.DesktopUnverified && !verification.DesktopStopped) ||
		(!verification.DockerUnverified && !verification.DockerUnreachable) ||
		(!verification.WSLUnverified && !verification.NoRunningWSL)
}

// postconditionState tells unverified apart from failed, which a bare false conflates.
func postconditionState(satisfied, unverified bool) string {
	if unverified {
		return "unverified"
	}
	if satisfied {
		return "true"
	}
	return "false"
}
