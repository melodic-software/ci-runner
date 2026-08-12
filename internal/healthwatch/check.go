package healthwatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

const jobsFileSafetyCapBytes = 8 << 20

type Finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Result struct {
	Healthy   bool      `json:"healthy"`
	CheckedAt time.Time `json:"checkedAt"`
	Findings  []Finding `json:"findings,omitempty"`
}

type WorkerInventory interface {
	RunningByPool(context.Context, string) (map[string]int, error)
}

type JobsFileStat interface {
	Size(context.Context) (int64, error)
}

type SidecarStore interface {
	Load(context.Context) (Sidecar, error)
	Save(context.Context, Sidecar) error
}

type Sidecar struct {
	WorkerDivergenceSince *time.Time `json:"workerDivergenceSince,omitempty"`
	LastAlertFingerprint  string     `json:"lastAlertFingerprint,omitempty"`
	LastAlertAt           *time.Time `json:"lastAlertAt,omitempty"`
}

type Alerter interface {
	Send(context.Context, Result) error
}

type Dependencies struct {
	Now       func() time.Time
	Config    config.Config
	Store     state.Store
	Inventory WorkerInventory
	JobsFile  JobsFileStat
	Sidecar   SidecarStore
	Alert     Alerter
}

type Checker struct {
	deps Dependencies
}

func NewChecker(deps Dependencies) (*Checker, error) {
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Store == nil {
		return nil, errors.New("state store is required")
	}
	if deps.Inventory == nil {
		return nil, errors.New("worker inventory is required")
	}
	if deps.JobsFile == nil {
		return nil, errors.New("jobs file stat is required")
	}
	if deps.Sidecar == nil {
		return nil, errors.New("health watch sidecar store is required")
	}
	if deps.Alert == nil {
		deps.Alert = NoopAlerter{}
	}
	return &Checker{deps: deps}, nil
}

func (c *Checker) Check(ctx context.Context) (Result, error) {
	now := c.deps.Now()
	result := Result{CheckedAt: now, Healthy: true}

	observed, observedErr := c.deps.Store.LoadObserved(ctx)
	if observedErr != nil && !errors.Is(observedErr, state.ErrNotFound) {
		return Result{}, fmt.Errorf("load observed state: %w", observedErr)
	}
	if observedErr != nil {
		result.Healthy = false
		result.Findings = append(result.Findings, Finding{
			Code:    "observed-missing",
			Message: "observed.json is missing",
		})
	} else {
		result.Findings = append(result.Findings, c.checkHeartbeat(now, observed)...)
	}

	running, err := c.deps.Inventory.RunningByPool(ctx, c.deps.Config.Host.ID)
	if err != nil {
		return Result{}, fmt.Errorf("inventory running workers: %w", err)
	}
	if observedErr == nil {
		divergenceFindings, sidecarUpdate, err := c.checkWorkerDivergence(ctx, now, observed, running)
		if err != nil {
			return Result{}, err
		}
		result.Findings = append(result.Findings, divergenceFindings...)
		if sidecarUpdate {
			// Sidecar persistence errors are non-fatal to the check itself.
		}
	}

	jobsFindings, err := c.checkJobsFileSize(ctx)
	if err != nil {
		return Result{}, err
	}
	result.Findings = append(result.Findings, jobsFindings...)

	if len(result.Findings) > 0 {
		result.Healthy = false
	}
	return result, nil
}

func (c *Checker) checkHeartbeat(now time.Time, observed model.ObservedState) []Finding {
	settings := c.deps.Config.HealthWatchdog
	if observed.HeartbeatAt.IsZero() {
		return []Finding{{
			Code:    "stale-heartbeat",
			Message: "observed heartbeatAt is missing",
		}}
	}
	threshold := c.deps.Config.Controller.ReconcileInterval.Duration * time.Duration(settings.HeartbeatStaleMultiplier)
	age := now.Sub(observed.HeartbeatAt)
	if age <= threshold {
		return nil
	}
	return []Finding{{
		Code: "stale-heartbeat",
		Message: fmt.Sprintf(
			"observed heartbeat is stale: heartbeatAt=%s age=%s threshold=%s phase=%s",
			observed.HeartbeatAt.Format(time.RFC3339),
			age.Round(time.Second),
			threshold,
			observed.Phase,
		),
	}}
}

func (c *Checker) checkWorkerDivergence(ctx context.Context, now time.Time, observed model.ObservedState, running map[string]int) ([]Finding, bool, error) {
	if !expectsRunningWorkers(observed) {
		sidecar, err := c.deps.Sidecar.Load(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("load health watch sidecar: %w", err)
		}
		if sidecar.WorkerDivergenceSince != nil {
			sidecar.WorkerDivergenceSince = nil
			if err := c.deps.Sidecar.Save(ctx, sidecar); err != nil {
				return nil, false, fmt.Errorf("clear health watch sidecar: %w", err)
			}
			return nil, true, nil
		}
		return nil, false, nil
	}

	diverged := false
	var details []string
	for _, pool := range observed.Pools {
		if pool.DesiredWorkers <= running[pool.ID] {
			continue
		}
		diverged = true
		details = append(details, fmt.Sprintf("%s desired=%d running=%d", pool.ID, pool.DesiredWorkers, running[pool.ID]))
	}

	sidecar, err := c.deps.Sidecar.Load(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("load health watch sidecar: %w", err)
	}
	updated := false
	if !diverged {
		if sidecar.WorkerDivergenceSince != nil {
			sidecar.WorkerDivergenceSince = nil
			updated = true
		}
		if updated {
			if err := c.deps.Sidecar.Save(ctx, sidecar); err != nil {
				return nil, false, fmt.Errorf("clear health watch sidecar: %w", err)
			}
		}
		return nil, updated, nil
	}

	if sidecar.WorkerDivergenceSince == nil {
		started := now
		sidecar.WorkerDivergenceSince = &started
		updated = true
	}
	if err := c.deps.Sidecar.Save(ctx, sidecar); err != nil {
		return nil, false, fmt.Errorf("save health watch sidecar: %w", err)
	}

	grace := c.deps.Config.HealthWatchdog.WorkerDivergenceGrace.Duration
	if sidecar.WorkerDivergenceSince != nil && now.Sub(*sidecar.WorkerDivergenceSince) >= grace {
		return []Finding{{
			Code: "worker-divergence",
			Message: fmt.Sprintf(
				"desired running workers exceed observed Docker inventory for %s (since %s, grace=%s)",
				joinDetails(details),
				sidecar.WorkerDivergenceSince.Format(time.RFC3339),
				grace,
			),
		}}, updated, nil
	}
	return nil, updated, nil
}

func (c *Checker) checkJobsFileSize(ctx context.Context) ([]Finding, error) {
	size, err := c.deps.JobsFile.Size(ctx)
	if err != nil {
		if errors.Is(err, ErrJobsFileMissing) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat jobs.json: %w", err)
	}
	threshold := int64(c.deps.Config.HealthWatchdog.JobsSizeWarningPercent) * jobsFileSafetyCapBytes / 100
	if size < threshold {
		return nil, nil
	}
	return []Finding{{
		Code: "jobs-size-warning",
		Message: fmt.Sprintf(
			"jobs.json size %d bytes is at or above the %d%% warning threshold (%d of %d bytes)",
			size,
			c.deps.Config.HealthWatchdog.JobsSizeWarningPercent,
			threshold,
			jobsFileSafetyCapBytes,
		),
	}}, nil
}

func expectsRunningWorkers(observed model.ObservedState) bool {
	switch observed.Phase {
	case model.PhaseReady, model.PhaseResourceConstrained, model.PhasePowerSuspended, model.PhaseStarting:
	default:
		return false
	}
	for _, pool := range observed.Pools {
		if pool.DesiredWorkers > 0 {
			return true
		}
	}
	return false
}

func joinDetails(details []string) string {
	if len(details) == 0 {
		return "no pools"
	}
	if len(details) == 1 {
		return details[0]
	}
	out := details[0]
	for _, detail := range details[1:] {
		out += "; " + detail
	}
	return out
}

type NoopAlerter struct{}

func (NoopAlerter) Send(context.Context, Result) error { return nil }
