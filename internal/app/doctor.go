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
		// desired=disabled with observed disabled is a legitimate idle host.
		// desired=enabled while observed is still disabled is the #277
		// never-ready wedge: the control plane answers, so
		// controller-control-plane PASSes, but reconcile has never reached a
		// serving phase. PhaseStarting is a normal enable/bootstrap window;
		// fail only after the configured startup budget.
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
			// Acknowledged zero while the planner still wants workers is the
			// #281 handshake wedge: GitHub accepted the advertisement, so the
			// acknowledgement bit PASSes, but the fleet is starved.
			// Planned quiesce/drain advertises zero while DesiredWorkers is
			// still positive; only a serving-ready pool is a handshake wedge.
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
		// Gaming probes carry their own per-probe deadlines, so the command
		// context is passed through: an aggregate budget here would re-create
		// the starvation the per-probe deadlines exist to remove.
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
			// An unverified postcondition is a gap in the observation, not
			// evidence that gaming mode failed, so it surfaces as WARN. An
			// observed violation still fails: only checks the probes could not
			// answer are advisory.
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
		// Inspection probes carry their own per-probe deadlines -- one of them
		// sized for a person answering a UAC prompt -- so the command context is
		// passed through: an aggregate budget here would re-create the starvation
		// the per-probe deadlines exist to remove, and would cap the elevated
		// probe below human speed because a derived context expires no later than
		// its parent.
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

// listenerAcknowledgementConvergenceLegs counts the GitHub request paths a
// capacity acknowledgement has to cross before this check can observe it: the
// controller advertising the new capacity, and a later reconcile reading back
// the pool state that acknowledges it.
const listenerAcknowledgementConvergenceLegs = 2

// listenerAcknowledgementGrace bounds how long a pool may sit with capacity
// advertised but unacknowledged before the doctor calls the listener unhealthy.
//
// The acknowledgement is not a protocol signal -- the scale-set protocol never
// acknowledges capacity back -- but this controller's own convergence check, so
// the window has to cover the request path that convergence actually travels.
// Each convergence leg is one full controller.RetryValue envelope: up to
// Retry.MaxAttempts attempts, each capped at RequestTimeout, separated by
// backoff waits sized at maxJitteredBackoff. So the bound is that complete
// envelope per leg, derived from the configured retry policy rather than from
// any observation of how long convergence has happened to take.
//
// It has to be the complete envelope, not a sample of it, because nothing else
// in the doctor notices a poll that is still legitimately retrying: while a poll
// is open, Reconciler.pollCheckpoint keeps writing observed state on the
// reconcile cadence, so the heartbeat stays fresh for observedFreshnessLimit
// while the pool transition timestamp this age measures stays deliberately
// pinned. A window shorter than the retry policy therefore hard-faults a
// listener whose configured retry sequence has not finished -- which is what
// budgeting a single request with no retry allowance did, and what let benign
// busy-fleet lag (2m9s, per the ci-runner-alignment audit's D4 finding) trip a
// hard fault. Deriving from the policy contains that observation with wide
// margin as a consequence, not as the calibration target.
//
// This is a bound on legitimate convergence, so it is unrelated to
// observedFreshnessLimit's bound on heartbeat staleness and carries no
// ordering against it: a transition legitimately spans several reconciles and
// several retry envelopes, while pollCheckpoint refreshes the heartbeat every
// reconcile interval. The cost of the wider window is detection latency, which
// now scales with the configured retry policy. Detection itself is unchanged: a
// wedged listener never acknowledges, so its lag grows monotonically past any
// bounded window and still hard-faults.
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

// observedFreshnessLimit bounds how stale the observed heartbeat may
// legitimately be on a healthy host: one full retry envelope per configured
// target plus two reconcile intervals. It is the one definition of
// "legitimately fresh" for that signal — the doctor's observed-state check
// uses it directly, and health-watch floors its stale-heartbeat threshold at
// it (wired in healthWatchCheck), so the two monitors cannot drift.
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

// saturatingFreshnessDuration bounds retryUnits attempts of a retryable GitHub
// request -- each costing request plus one retryBackoff wait -- plus the two
// reconcile intervals a caller needs to observe the result, clamping to the
// largest representable time.Duration instead of overflowing. Callers compose
// retryUnits from their own attempt and repetition counts.
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

// postconditionState renders three states where a bare bool renders two. An
// unchecked postcondition and a failed one are both `false`, and an operator
// reading `desktopStopped=false` cannot otherwise tell "still running" from
// "the probe never answered".
func postconditionState(satisfied, unverified bool) string {
	if unverified {
		return "unverified"
	}
	if satisfied {
		return "true"
	}
	return "false"
}
