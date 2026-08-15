package healthwatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/jobindex"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

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

// JobsSnapshot is one consistent observation of jobs.json: the byte size on
// disk and the decoded catalog behind it.
type JobsSnapshot struct {
	SizeBytes int64
	Catalog   jobindex.Catalog
}

type JobsFileStat interface {
	Snapshot(context.Context) (JobsSnapshot, error)
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
	// HeartbeatFreshnessFloor is the derived bound on how stale the observed
	// heartbeat may legitimately be on a healthy host; the app wires it from
	// the same derivation doctor's observed-state check uses, so the two
	// monitors can never disagree about what "legitimately fresh" means. The
	// stale-heartbeat threshold never drops below it (see checkHeartbeat).
	HeartbeatFreshnessFloor time.Duration
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

	running, inventoryErr := c.deps.Inventory.RunningByPool(ctx, c.deps.Config.Host.ID)
	if inventoryErr != nil {
		result.Healthy = false
		result.Findings = append(result.Findings, Finding{
			Code:    "inventory-unavailable",
			Message: fmt.Sprintf("worker inventory unavailable: %v", inventoryErr),
		})
	} else if observedErr == nil {
		divergenceFindings, err := c.checkWorkerDivergence(ctx, now, observed, running)
		if err != nil {
			return Result{}, err
		}
		result.Findings = append(result.Findings, divergenceFindings...)
	}

	jobsFindings, err := c.checkJobsIndex(ctx)
	if err != nil {
		return Result{}, err
	}
	result.Findings = append(result.Findings, jobsFindings...)

	if len(result.Findings) > 0 {
		result.Healthy = false
	}
	return result, nil
}

// checkHeartbeat thresholds heartbeat age at reconcileInterval times
// heartbeatStaleMultiplier, floored at the host's derived freshness bound.
// Heartbeat age scales with load, not just the reconcile cadence: reconcile
// passes legitimately stretch under CPU contention and GitHub retries (24s
// observed at a 5s interval while a job ran, #270), so a threshold keyed to
// the idle cadence alone alarms exactly when the host is doing its job — the
// same chronic-false-alarm class #262 removed from the jobs-size signal. The
// floor shifts detection latency, not detection: a wedged controller never
// heartbeats again, so its age grows monotonically past any bounded threshold.
func (c *Checker) checkHeartbeat(now time.Time, observed model.ObservedState) []Finding {
	settings := c.deps.Config.HealthWatchdog
	if observed.HeartbeatAt.IsZero() {
		return []Finding{{
			Code:    "stale-heartbeat",
			Message: "observed heartbeatAt is missing",
		}}
	}
	threshold := c.deps.Config.Controller.ReconcileInterval.Duration * time.Duration(settings.HeartbeatStaleMultiplier)
	threshold = max(threshold, c.deps.HeartbeatFreshnessFloor)
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

func (c *Checker) checkWorkerDivergence(ctx context.Context, now time.Time, observed model.ObservedState, running map[string]int) ([]Finding, error) {
	if !expectsRunningWorkers(observed) {
		sidecar, err := c.deps.Sidecar.Load(ctx)
		if err != nil {
			return nil, fmt.Errorf("load health watch sidecar: %w", err)
		}
		if sidecar.WorkerDivergenceSince != nil {
			sidecar.WorkerDivergenceSince = nil
			if err := c.deps.Sidecar.Save(ctx, sidecar); err != nil {
				return nil, fmt.Errorf("clear health watch sidecar: %w", err)
			}
		}
		return nil, nil
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
		return nil, fmt.Errorf("load health watch sidecar: %w", err)
	}
	if !diverged {
		if sidecar.WorkerDivergenceSince != nil {
			sidecar.WorkerDivergenceSince = nil
			if err := c.deps.Sidecar.Save(ctx, sidecar); err != nil {
				return nil, fmt.Errorf("clear health watch sidecar: %w", err)
			}
		}
		return nil, nil
	}

	if sidecar.WorkerDivergenceSince == nil {
		started := now
		sidecar.WorkerDivergenceSince = &started
	}
	if err := c.deps.Sidecar.Save(ctx, sidecar); err != nil {
		return nil, fmt.Errorf("save health watch sidecar: %w", err)
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
		}}, nil
	}
	return nil, nil
}

// checkJobsIndex watches the two jobs.json shapes capacity compaction cannot
// absorb. Total file size is deliberately not a finding: at steady-state job
// churn the save loop pins the file at the cap by design, reclaiming
// tombstoned and terminal records on every write, so a size-keyed warning
// fires forever on a healthy host. Only records the compaction tiers may
// never drop — open, or awaiting a terminal marker — can wedge index writes,
// so the warning threshold applies to their encoded size alone.
func (c *Checker) checkJobsIndex(ctx context.Context) ([]Finding, error) {
	snapshot, err := c.deps.JobsFile.Snapshot(ctx)
	if err != nil {
		if errors.Is(err, ErrJobsFileMissing) {
			return nil, nil
		}
		if errors.Is(err, ErrJobsFileUndecodable) {
			return []Finding{{
				Code:    "jobs-index-undecodable",
				Message: err.Error(),
			}}, nil
		}
		return nil, fmt.Errorf("read jobs.json: %w", err)
	}

	var findings []Finding
	if snapshot.SizeBytes > jobindex.MaximumJobStateBytes {
		findings = append(findings, Finding{
			Code: "jobs-size-overflow",
			Message: fmt.Sprintf(
				"jobs.json size %d bytes exceeds the %d-byte save cap the store never commits above; the next successful save compacts it, so a persistent overflow means index writes are not landing",
				snapshot.SizeBytes,
				jobindex.MaximumJobStateBytes,
			),
		})
	}

	live := jobindex.Catalog{SchemaVersion: snapshot.Catalog.SchemaVersion}
	for _, record := range snapshot.Catalog.Records {
		if !jobindex.CompactableUnderCapacityPressure(record) {
			live.Records = append(live.Records, record)
		}
	}
	liveBytes, err := jobindex.EncodedCatalogSize(live)
	if err != nil {
		return nil, fmt.Errorf("measure non-compactable jobs.json records: %w", err)
	}
	threshold := c.deps.Config.HealthWatchdog.JobsSizeWarningPercent * jobindex.MaximumJobStateBytes / 100
	if liveBytes >= threshold {
		findings = append(findings, Finding{
			Code: "jobs-live-size-warning",
			Message: fmt.Sprintf(
				"non-compactable jobs.json records (open or awaiting a terminal marker) encode to %d bytes, at or above the %d%% warning threshold (%d of %d bytes); %d of %d records cannot be reclaimed by capacity compaction",
				liveBytes,
				c.deps.Config.HealthWatchdog.JobsSizeWarningPercent,
				threshold,
				jobindex.MaximumJobStateBytes,
				len(live.Records),
				len(snapshot.Catalog.Records),
			),
		})
	}
	return findings, nil
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
