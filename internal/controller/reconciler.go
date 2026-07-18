package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/scaleset"
	statepkg "github.com/melodic-software/ci-runner/internal/state"
	"github.com/melodic-software/ci-runner/internal/telemetry"
)

var ErrUnsafeObservedState = errors.New("observed state could not be loaded; refusing to reconcile persistent scale-set identities")

var (
	errReconcileInputsChanged = errors.New("reconciliation safety inputs changed")
	errShutdownRequested      = errors.New("controller shutdown requested")
)

type Dependencies struct {
	ScaleSets ScaleSetClient
	Workers   WorkerRuntime
	Desktop   DesktopManager
	Power     PowerMonitor
	Resources ResourceMonitor
	State     StateStore
	Jobs      JobLookup
	Clock     Clock
	Logs      LogSink
	Telemetry telemetry.Recorder
	// EngineMemory cross-checks a configured worker memory budget against the
	// engine VM's real total memory. Optional: absent, the budget is trusted
	// as configured.
	EngineMemory EngineMemoryProbe
}

type Reconciler struct {
	config  config.Config
	version string
	deps    Dependencies

	// stepMu serializes side effects without blocking local control-plane status
	// requests. stateMu protects only short-lived control state; capacityMu
	// protects only the pending-capacity transition map. Neither may be held
	// across an adapter call or listener poll.
	stepMu     sync.Mutex
	stateMu    sync.Mutex
	capacityMu sync.Mutex

	backoff           BackoffPolicy
	watchInterval     time.Duration
	currentStepCancel context.CancelCauseFunc
	drainCapacity     map[string]int
	sequence          uint64
	shuttingDown      bool
	pendingCapacity   map[string]int

	// engineMemoryTotal caches the probed engine VM MemTotal for the current
	// VM lifecycle. Only step() and its same-goroutine callees touch it while
	// stepMu is held; the poll-cadence goroutine receives the value by copy in
	// pollCadenceState.
	engineMemoryTotal uint64

	// registrationCheckCursor rotates which idle workers' GitHub registration
	// gets verified when there are more eligible candidates than one Step's
	// registrationCheckCap allows (see step()). It is only ever read and
	// written from within step(), itself only reachable while stepMu is held,
	// so unlike drainCapacity/sequence it needs no additional lock.
	registrationCheckCursor uint64

	// retirementCursor rotates which worker in plan.Remove is tried first when
	// there are more retirement-eligible candidates than one Step's
	// retirementDeregistrationCap allows (see step()), so a worker whose
	// deregisterRunner call keeps failing cannot permanently starve every
	// worker behind it in plan.Remove. Like registrationCheckCursor, it is
	// only ever read and written from within step() while stepMu is held.
	retirementCursor uint64
}

type ReconcileResult struct {
	Observed           model.ObservedState
	Plan               Plan
	CheckpointAge      time.Duration
	CheckpointAgeValid bool
}

func NewReconciler(cfg config.Config, version string, deps Dependencies) (*Reconciler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.ScaleSets == nil || deps.Workers == nil || deps.Desktop == nil || deps.Power == nil || deps.Resources == nil || deps.State == nil || deps.Jobs == nil || deps.Clock == nil {
		return nil, errors.New("controller dependencies must not be nil")
	}
	if deps.Logs == nil {
		deps.Logs = DiscardLogSink{}
	}
	if deps.Telemetry == nil {
		deps.Telemetry = telemetry.Noop()
	}
	if deps.EngineMemory == nil {
		deps.EngineMemory = unknownEngineMemory{}
	}
	if version == "" {
		version = buildinfo.Version
	}
	watchInterval := cfg.Controller.ReconcileInterval.Duration
	if watchInterval <= 0 || watchInterval > time.Second {
		watchInterval = time.Second
	}
	return &Reconciler{
		config: cfg, version: version, deps: deps,
		backoff: BackoffPolicy{
			Initial: cfg.GitHub.Retry.Initial.Duration, Maximum: cfg.GitHub.Retry.Maximum.Duration,
			Multiplier: cfg.GitHub.Retry.Multiplier, JitterRatio: cfg.GitHub.Retry.JitterRatio,
			MaxAttempts: cfg.GitHub.Retry.MaxAttempts, Jitter: cryptoJitter,
		},
		watchInterval:   watchInterval,
		drainCapacity:   make(map[string]int),
		pendingCapacity: make(map[string]int),
	}, nil
}

func (r *Reconciler) SetBackoffForTest(policy BackoffPolicy) error {
	if err := policy.validate(); err != nil {
		return err
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.backoff = policy
	return nil
}

func (r *Reconciler) setWatchIntervalForTest(interval time.Duration) error {
	if interval <= 0 {
		return errors.New("safety watch interval must be positive")
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.watchInterval = interval
	return nil
}

// EffectiveMaximumConcurrentWorkers reports the host-wide worker cap the next
// Step's BuildPlan will apply, honoring any active
// Desired.TemporaryCapacityOverride. internal/app's per-step watchdog budget
// calls this before starting each Step so a legitimate temporary scale-up
// widens the JIT-start portion of the budget instead of tripping the watchdog
// on a policy-compliant burst reconcile. A desired-state read failure fails
// safe to the static configured cap -- the same value step() itself falls
// back to when it cannot load desired state -- rather than assuming an
// override might be in effect that cannot be verified.
func (r *Reconciler) EffectiveMaximumConcurrentWorkers(ctx context.Context) int {
	desired, err := r.deps.State.LoadDesired(ctx)
	if err != nil {
		return r.config.Resources.MaximumConcurrentWorkers
	}
	return EffectiveMaximumConcurrentWorkers(r.config.Resources, desired)
}

// Step performs one serialized reconciliation. Polling scale-set statistics
// (and therefore advertising capacity) happens before any idle worker removal.
func (r *Reconciler) Step(ctx context.Context) (result ReconcileResult, resultErr error) {
	r.stepMu.Lock()
	defer r.stepMu.Unlock()
	ctx, finishTelemetry := r.deps.Telemetry.BeginReconcile(ctx)
	defer func() { finishTelemetry(telemetrySnapshot(result), resultErr) }()

	for {
		if err := ctx.Err(); err != nil {
			return ReconcileResult{}, err
		}
		stepCtx, cancel := context.WithCancelCause(ctx)
		r.stateMu.Lock()
		r.currentStepCancel = cancel
		r.stateMu.Unlock()

		result, err := r.step(stepCtx, cancel)
		cause := context.Cause(stepCtx)
		r.stateMu.Lock()
		r.currentStepCancel = nil
		r.stateMu.Unlock()
		cancel(nil)

		if errors.Is(cause, errReconcileInputsChanged) && ctx.Err() == nil {
			// Re-run immediately so the changed desired/power state is advertised;
			// waiting for the normal reconciliation interval could leave stale
			// nonzero capacity visible for an entire long poll.
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ReconcileResult{}, ctxErr
		}
		if cause != nil && err == nil {
			err = cause
		}
		return result, err
	}
}

func telemetrySnapshot(result ReconcileResult) telemetry.ReconcileSnapshot {
	observed := result.Observed
	snapshot := telemetry.ReconcileSnapshot{
		Valid: observed.SchemaVersion > 0, Phase: string(observed.Phase),
		CPUPercent:           observed.Resources.CPUUtilizationPercent,
		AvailableMemoryBytes: observed.Resources.AvailableMemoryBytes,
		MemoryHeadroomBytes:  result.Plan.MemoryHeadroom,
		ResourceGateBlocked:  observed.ResourceGate.Blocked,
		PowerGateBlocked:     result.Plan.Phase == model.PhasePowerSuspended,
		CheckpointAge:        result.CheckpointAge,
		CheckpointAgeValid:   result.CheckpointAgeValid,
		Pools:                make([]telemetry.ReconcilePool, 0, len(observed.Pools)),
		Workers:              make([]telemetry.ReconcileWorker, 0, len(observed.Workers)),
	}
	for _, pool := range observed.Pools {
		acknowledgementAgeValid := !pool.UpdatedAt.IsZero() && !observed.HeartbeatAt.Before(pool.UpdatedAt)
		snapshot.Pools = append(snapshot.Pools, telemetry.ReconcilePool{
			ID: pool.ID, Advertised: pool.MaxCapacity,
			Assigned: pool.TotalAssignedJobs, Desired: pool.DesiredWorkers,
			AffordableWorkers:              result.Plan.MemoryAffordable[pool.ID],
			CapacityAcknowledged:           pool.CapacityAcknowledged,
			AcknowledgementPendingAge:      observed.HeartbeatAt.Sub(pool.UpdatedAt),
			AcknowledgementPendingAgeValid: acknowledgementAgeValid,
		})
	}
	for _, worker := range observed.Workers {
		snapshot.Workers = append(snapshot.Workers, telemetry.ReconcileWorker{PoolID: worker.PoolID, State: string(worker.State)})
	}
	return snapshot
}

func (r *Reconciler) step(ctx context.Context, cancel context.CancelCauseFunc) (ReconcileResult, error) {
	if err := ctx.Err(); err != nil {
		return ReconcileResult{}, err
	}

	now := r.deps.Clock.Now().UTC()
	desired, err := r.deps.State.LoadDesired(ctx)
	var desiredLoadErr error
	if errors.Is(err, statepkg.ErrNotFound) {
		// Missing local intent fails safe. Provisioning may create an initial
		// desired file, but it must never overwrite one after installation.
		desired = model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled, UpdatedAt: now}
		if err := r.deps.State.SaveDesired(ctx, desired); err != nil {
			return ReconcileResult{}, fmt.Errorf("initialize desired state: %w", err)
		}
	} else if err != nil {
		// A desired-state read failure must fail capacity closed. Preserve the
		// file as evidence, but continue with transient disabled intent so any
		// existing listener can acknowledge zero capacity.
		desiredLoadErr = fmt.Errorf("load desired state: %w", err)
		desired = model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled, UpdatedAt: now}
	}
	if r.isShuttingDown() {
		// Process replacement drains transiently without changing the user's
		// persisted intent. The replacement process can resume that intent.
		desired.Mode = model.ModeDisabled
		desired.TemporaryCapacityOverride = nil
	}

	previous, err := r.deps.State.LoadObserved(ctx)
	checkpointAge := time.Duration(0)
	checkpointAgeValid := err == nil && !previous.HeartbeatAt.IsZero() && !previous.HeartbeatAt.After(now)
	if checkpointAgeValid {
		checkpointAge = now.Sub(previous.HeartbeatAt)
	}
	recoveryOnly := false
	var observedLoadErr error
	if err != nil && !errors.Is(err, statepkg.ErrNotFound) {
		quarantiner, ok := r.deps.State.(interface{ QuarantineObserved(context.Context) error })
		if !ok {
			return ReconcileResult{}, errors.Join(ErrUnsafeObservedState, err)
		}
		if quarantineErr := quarantiner.QuarantineObserved(ctx); quarantineErr != nil {
			return ReconcileResult{}, errors.Join(ErrUnsafeObservedState, err, quarantineErr)
		}
		// Preserve the corrupt source until SaveObserved atomically replaces it,
		// then reconstruct the exact configured scale set solely to advertise
		// zero. No worker or Desktop lifecycle mutation is allowed in this pass.
		observedLoadErr = err
		previous = model.ObservedState{}
		recoveryOnly = true
		desired.Mode = model.ModeDisabled
		desired.TemporaryCapacityOverride = nil
	}

	var operationErrors []error
	var operationProblems []model.Problem
	record := func(code, message, poolID string, retryable bool, err error) {
		operationProblems = append(operationProblems, problem(now, code, message, poolID, retryable))
		event := LogEvent{At: now, Code: code, Message: message, PoolID: poolID}
		if err != nil {
			operationErrors = append(operationErrors, err)
			event.Cause = err.Error()
		}
		r.writeLog(ctx, event)
	}
	note := func(code, message, poolID string) {
		r.writeLog(ctx, LogEvent{At: now, Code: code, Message: message, PoolID: poolID})
	}
	if desiredLoadErr != nil {
		record("desired-state-error", "desired state could not be read; capacity is held at zero", "", true, desiredLoadErr)
	}
	if observedLoadErr != nil {
		record("observed-state-recovered", "corrupt observed state was quarantined; exact configured listeners are held at zero and lifecycle mutations are suspended", "", false, observedLoadErr)
	}

	observationFailed := false
	power, powerErr := r.deps.Power.Snapshot(ctx)
	if powerErr != nil {
		observationFailed = true
		record("power-monitor-error", "power observation failed; new work is blocked", "", true, powerErr)
	}
	// This watcher is a child of the Step context, which the reconcileStepTimeout
	// watchdog already bounds, so it needs no deadline of its own. A per-request
	// deadline here would wrongly expire it during a normal multi-attempt listener
	// poll: r.statistics runs through RetryValue for up to Retry.MaxAttempts
	// attempts, so the poll it shadows can legitimately span minutes.
	watchContext, stopWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func(watchedDesired model.DesiredState, watchedPower model.PowerSnapshot, forcedZero bool) {
		defer close(watchDone)
		r.watchSafetyInputs(watchContext, watchedDesired, watchedPower, cancel, forcedZero)
	}(desired, power, recoveryOnly || desiredLoadErr != nil || powerErr != nil || r.isShuttingDown())
	defer func() {
		stopWatch()
		<-watchDone
	}()
	desktop, desktopErr := r.deps.Desktop.Status(ctx)
	desktopStatusKnown := desktopErr == nil
	if desktopErr != nil {
		observationFailed = true
		record("desktop-status-error", "Docker Desktop status failed; new work is blocked", "", true, desktopErr)
	}

	// Desktop lifecycle is a host prerequisite, not a worker-admission decision.
	// A known stopped/unreachable engine cannot answer Docker inventory calls, so
	// enabled mode must bootstrap it before resource admission or inventory. An
	// unknown Desktop status and an unknown power state still fail closed.
	if desired.Mode == model.ModeEnabled && desktopStatusKnown && powerErr == nil {
		_, powerAllowed := evaluatePowerGate(previous.PowerGate, power, r.config.Power, now)
		if powerAllowed && (!desktop.DesktopRunning || !desktop.EngineReachable) {
			if startErr := r.deps.Desktop.Start(ctx, r.config.DockerDesktop.StartTimeout.Duration); startErr != nil {
				observationFailed = true
				record("desktop-start-error", "Docker Desktop did not start within the configured policy", "", true, startErr)
			} else if refreshed, statusErr := r.deps.Desktop.Status(ctx); statusErr != nil {
				desktopStatusKnown = false
				observationFailed = true
				record("desktop-start-status-error", "Docker Desktop status could not be verified after startup", "", true, statusErr)
			} else {
				desktop = refreshed
				if !desktop.DesktopRunning || !desktop.EngineReachable {
					observationFailed = true
					verificationErr := errors.New("desktop remained stopped or its Docker engine remained unreachable after startup")
					record("desktop-start-verification-error", "Docker Desktop did not reach a usable state after startup", "", true, verificationErr)
				}
			}
		}
	}

	r.probeEngineMemory(ctx, desktopStatusKnown, desktop, note)

	resources, resourceErr := r.deps.Resources.Snapshot(ctx)
	if resourceErr != nil {
		observationFailed = true
		record("resource-monitor-error", "resource observation failed; new work is blocked", "", true, resourceErr)
	}
	workers := []model.Worker(nil)
	if mayHaveManagedWorkers(desktop, desktopStatusKnown) {
		latest, listErr := r.deps.Workers.List(ctx)
		if listErr != nil {
			observationFailed = true
			record("worker-inventory-error", "managed-worker inventory failed; new work is blocked", "", true, listErr)
		} else {
			workers = latest
		}
	}
	// The host resource monitor reads physical RAM and CPU independent of Docker,
	// so a valid observation stays valid even when a Docker-side probe fails. When
	// the desktop is known to be down (stopped by gaming teardown, or not yet
	// started), preserve that real observation rather than zeroing it: an invalid
	// snapshot would trip evaluateResourceGate closed and block the StartDesktop
	// bootstrap that re-enable depends on, and no worker scheduling happens while
	// the desktop is down anyway. A running or unknown-state desktop still fails
	// closed, so a stale inventory can never admit work against an empty snapshot.
	desktopKnownDown := desktopStatusKnown && !desktop.DesktopRunning && !desktop.EngineReachable
	if observationFailed && !desktopKnownDown {
		resources = model.ResourceSnapshot{} // invalid observation fails closed in BuildPlan
	}
	jobStateKnown := true
	if enriched, lookupErr := r.enrichWorkerJobs(ctx, workers); lookupErr != nil {
		jobStateKnown = false
		observationFailed = true
		resources = model.ResourceSnapshot{}
		record("job-index-error", "durable job lifecycle state could not be read; new work and automatic retirement are blocked", "", true, lookupErr)
	} else {
		workers = enriched
	}

	previousPools := make(map[string]model.PoolObservation, len(previous.Pools))
	for _, pool := range previous.Pools {
		previousPools[pool.ID] = pool
	}
	identities := make(map[string]scaleset.Identity, len(r.config.GitHub.Targets))
	pools := make([]PoolSnapshot, 0, len(r.config.GitHub.Targets))
	for _, target := range r.config.GitHub.Targets {
		var prior *scaleset.Identity
		if saved, ok := previousPools[target.ID]; ok && saved.ScaleSetID > 0 && saved.ListenerID != "" {
			value := scaleset.Identity{ScaleSetID: saved.ScaleSetID, ListenerID: saved.ListenerID}
			prior = &value
		}
		identity, ensureErr := r.ensure(ctx, target, prior)
		if ensureErr != nil {
			record("scale-set-ensure-error", safeScaleSetMessage("ensure", ensureErr), target.ID, scaleset.Retryable(ensureErr), ensureErr)
			pools = append(pools, PoolSnapshot{TargetID: target.ID})
			continue
		}
		identities[target.ID] = identity
		assigned := previousPools[target.ID].TotalAssignedJobs
		drainCapacity := previousPools[target.ID].DrainServiceCapacity
		if drainCapacity == 0 && previousPools[target.ID].MaxCapacity > 0 {
			drainCapacity = previousPools[target.ID].MaxCapacity
		}
		if remembered := r.rememberedDrainCapacity(target.ID); remembered > drainCapacity {
			drainCapacity = remembered
		}
		pools = append(pools, PoolSnapshot{
			TargetID: target.ID, Identity: identity, TotalAssignedJobs: assigned,
			DrainServiceCapacity: drainCapacity, Ready: true,
		})
	}

	// Compute capacity from the last authoritative statistics, then send that
	// capacity with this poll. Newly returned statistics drive worker changes
	// now and the next poll's capacity, avoiding an unsupported reservation API.
	// A withdrawal-triggered rerun reaches this call with a checkpoint that
	// still reports the last acknowledged capacity, not the capacity the
	// canceled poll had in flight; carry that in-flight baseline forward so
	// the rerun holds a still-affordable remainder instead of re-deriving it
	// as fresh growth.
	provisional := BuildPlan(PlanInput{
		Config: r.config, Desired: desired, Previous: previous, CapacityHysteresis: r.pendingCapacitySnapshot(), Pools: pools,
		Workers: workers, Resources: resources, Power: power, Desktop: desktop,
		EngineMemoryTotalBytes: r.engineMemoryTotal, Now: now,
	})
	pollPlan := provisional
	pollPlan.AdvertisedCapacity = sequenceCapacityTransfer(previous, provisional.AdvertisedCapacity)
	checkpoint := r.pollCheckpoint(previous, pools, workers, resources, power, desktop, pollPlan, r.deps.Clock.Now().UTC(), operationProblems)
	var (
		stopPollWatch context.CancelFunc
		pollWatchDone chan pollCadenceResult
	)
	if containsReadyPool(pools) {
		checkpointErr := r.deps.State.SaveObserved(ctx, checkpoint)
		// Child of the Step context bounded by the reconcileStepTimeout watchdog. A
		// separate per-request deadline would expire this cadence watcher during a
		// normal multi-attempt poll retry sequence, so none is set here.
		pollWatchContext, stop := context.WithCancel(ctx)
		stopPollWatch = stop
		pollWatchDone = make(chan pollCadenceResult, 1)
		cadenceState := pollCadenceState{
			desired: desired, observed: checkpoint, pools: pools, workers: workers, desktop: desktop,
			advertised: pollPlan.AdvertisedCapacity, operationProblems: operationProblems,
			engineMemoryTotal: r.engineMemoryTotal,
			forcedZero:        recoveryOnly || desiredLoadErr != nil || observationFailed || r.isShuttingDown() || desired.Mode != model.ModeEnabled,
			checkpointErr:     checkpointErr,
		}
		go func() {
			pollWatchDone <- r.watchPollCadence(pollWatchContext, cancel, cadenceState)
		}()
	}
	type pollResult struct {
		index      int
		stats      scaleset.Statistics
		identity   scaleset.Identity
		advertised int
		err        error
	}
	pollResults := make(chan pollResult, len(pools))
	acknowledgedCapacity := make(map[string]int, len(pools))
	capacityAcknowledged := make(map[string]bool, len(pools))
	zeroAcknowledged := make(map[string]bool, len(pools))
	zeroConfirmations := make(map[string]int, len(pools))
	var polls sync.WaitGroup
	for index := range pools {
		pool := pools[index]
		if !pool.Ready {
			continue
		}
		polls.Add(1)
		go func(index int, pool PoolSnapshot) {
			defer polls.Done()
			advertised := pollPlan.AdvertisedCapacity[pool.TargetID]
			stats, identity, statsErr := r.statistics(ctx, r.target(pool.TargetID), pool.Identity, advertised)
			pollResults <- pollResult{index: index, stats: stats, identity: identity, advertised: advertised, err: statsErr}
		}(index, pool)
	}
	polls.Wait()
	cadenceResult := pollCadenceResult{observed: checkpoint}
	if stopPollWatch != nil {
		stopPollWatch()
		cadenceResult = <-pollWatchDone
	}
	resources = cadenceResult.observed.Resources
	power = cadenceResult.observed.Power
	previous.ResourceGate = cadenceResult.observed.ResourceGate
	previous.PowerGate = cadenceResult.observed.PowerGate
	cadencePools := make(map[string]model.PoolObservation, len(cadenceResult.observed.Pools))
	for _, pool := range cadenceResult.observed.Pools {
		cadencePools[pool.ID] = pool
	}
	now = r.deps.Clock.Now().UTC()
	close(pollResults)
	for result := range pollResults {
		pool := &pools[result.index]
		if result.err != nil {
			pool.Ready = false
			if pollSuperseded(result.err) {
				// The cadence watcher cancels an open listener long poll on purpose
				// (errReconcileInputsChanged) when reconciliation safety inputs
				// change, and Step reruns. The in-flight poll then unblocks with
				// context.Canceled: routine control flow, not a scale-set failure,
				// so it must not surface an error-level record or problem entry.
				continue
			}
			record("scale-set-statistics-error", safeScaleSetMessage("poll", result.err), pool.TargetID, scaleset.Retryable(result.err), result.err)
			continue
		}
		pool.Identity = result.identity
		identities[pool.TargetID] = result.identity
		pool.TotalAssignedJobs = result.stats.TotalAssignedJobs
		if result.advertised > 0 {
			pool.DrainServiceCapacity = result.advertised
			r.rememberDrainCapacity(pool.TargetID, result.advertised)
		} else if result.stats.TotalAssignedJobs == 0 {
			pool.DrainServiceCapacity = 0
			r.rememberDrainCapacity(pool.TargetID, 0)
		}
		acknowledgedCapacity[pool.TargetID] = result.advertised
		capacityAcknowledged[pool.TargetID] = true
		zeroAcknowledged[pool.TargetID] = result.advertised == 0
		if result.advertised == 0 && result.stats.TotalAssignedJobs == 0 {
			zeroConfirmations[pool.TargetID] = 1
			prior := previousPools[pool.TargetID]
			if prior.CapacityAcknowledged && prior.MaxCapacity == 0 && prior.TotalAssignedJobs == 0 && prior.ZeroCapacityConfirmations > 0 {
				zeroConfirmations[pool.TargetID] = minInt(2, prior.ZeroCapacityConfirmations+1)
			}
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return ReconcileResult{}, cause
	}
	if cadenceResult.checkpointErr != nil {
		record("reconcile-checkpoint-error", "controller heartbeat checkpoint failed during the listener poll", "", true, cadenceResult.checkpointErr)
	}

	// Refresh after capacity was advertised. This is important during drain:
	// an assignment accepted just before zero capacity must be observed as busy
	// and protected before any conditional removal is attempted.
	if mayHaveManagedWorkers(desktop, desktopStatusKnown) {
		if latest, listErr := r.deps.Workers.List(ctx); listErr != nil {
			jobStateKnown = false
			resources = model.ResourceSnapshot{}
			record("worker-refresh-error", "managed-worker refresh failed after capacity update; new work is blocked", "", true, listErr)
		} else if enriched, lookupErr := r.enrichWorkerJobs(ctx, latest); lookupErr != nil {
			jobStateKnown = false
			resources = model.ResourceSnapshot{}
			record("job-index-refresh-error", "durable job lifecycle state could not be refreshed after the capacity update; automatic retirement is blocked", "", true, lookupErr)
		} else {
			workers = enriched
		}
	}

	// A JIT runner can be canceled after GitHub assigns it but before the runner
	// acquires the job. GitHub then removes the one-job registration while the
	// stock runner process can remain alive with the hook state still at idle.
	// Verify only exact, job-free idle identities. An authoritative missing
	// registration makes that container unusable capacity; every lookup error
	// and every active/racing worker remains preserved.
	//
	// r.runnerRegistered runs a full GitHub RetryValue budget per call
	// (internal/app's reconcileStepTimeout budgets exactly
	// registrationCheckCap of these per Step). Unlike JIT starts, the number of
	// eligible idle workers here is not itself bounded by
	// MaximumConcurrentWorkers -- idle inventory accumulates independently of
	// that cap -- so cap the checks actually issued in this Step and rotate
	// which candidates get picked via registrationCheckCursor, deferring the
	// remainder (logged as worker-registration-check-deferred-step-budget) to
	// later Steps. A fixed from-the-front cap would starve candidates past the
	// cap forever whenever the same workers keep sorting first; rotating the
	// starting point guarantees every candidate is eventually checked as long
	// as new idle inventory does not outpace the cap indefinitely, the same
	// assumption the retirement deregistration cap above already relies on.
	if jobStateKnown {
		var candidates []int
		for index := range workers {
			worker := &workers[index]
			if worker.State != model.WorkerIdle || worker.JobID != "" || worker.RunnerID <= 0 {
				continue
			}
			if pool := findPool(pools, worker.PoolID); !pool.Ready {
				continue
			}
			candidates = append(candidates, index)
		}
		registrationCheckCap := max(r.config.Resources.MaximumConcurrentWorkers, 1)
		if n := len(candidates); n > 0 {
			start := int(r.registrationCheckCursor % uint64(n))
			checked := 0
			for i := 0; i < n; i++ {
				worker := &workers[candidates[(start+i)%n]]
				if checked >= registrationCheckCap {
					note("worker-registration-check-deferred-step-budget", "registration verification was deferred to a later reconcile step because this step already reached its per-step registration-check budget", worker.PoolID)
					continue
				}
				checked++
				registered, registrationErr := r.runnerRegistered(ctx, worker.PoolID, worker.RunnerID, worker.Name)
				if registrationErr != nil {
					record("runner-registration-check-error", safeScaleSetMessage("verify runner registration", registrationErr), worker.PoolID, scaleset.Retryable(registrationErr), registrationErr)
					continue
				}
				if !registered {
					worker.State = model.WorkerUnregistered
					r.writeLog(ctx, LogEvent{At: now, Code: "runner-registration-missing", Message: "idle worker registration no longer exists and will be retired", PoolID: worker.PoolID, WorkerID: worker.ID})
				}
			}
			r.registrationCheckCursor += uint64(checked)
		}
	}

	plan := BuildPlan(PlanInput{
		Config: r.config, Desired: desired, Previous: previous, Pools: pools,
		Workers: workers, Resources: resources, Power: power, Desktop: desktop,
		EngineMemoryTotalBytes: r.engineMemoryTotal, Now: now,
	})
	if recoveryOnly {
		plan.Start = nil
		plan.Remove = nil
		plan.StartDesktop = false
		plan.StopDesktop = false
		plan.ShutdownWSL = false
	}

	if plan.StartDesktop {
		if startErr := r.deps.Desktop.Start(ctx, r.config.DockerDesktop.StartTimeout.Duration); startErr != nil {
			record("desktop-start-error", "Docker Desktop did not start within the configured policy", "", true, startErr)
		}
	}

	// deregisterRunner runs a full GitHub RetryValue budget per call
	// (internal/app's reconcileStepTimeout budgets exactly
	// Resources.MaximumConcurrentWorkers worth of these per Step, mirroring the
	// JIT-start budget). Unlike JIT starts, plan.Remove's idle-worker count is
	// not itself bounded by MaximumConcurrentWorkers: lowering that setting or
	// warm capacity can legitimately leave more existing idle workers to drain
	// than the new cap allows for. Cap the deregisterRunner calls issued in this
	// Step at that same limit and defer any remainder to a later Step (where
	// plan.Remove is recomputed and picks the deferred workers back up) instead
	// of letting an unbounded retirement count exceed the watchdog's budget.
	retirementDeregistrationCap := max(r.config.Resources.MaximumConcurrentWorkers, 1)
	retirementDeregistrations := 0

	// Rotate which worker in plan.Remove is tried first each Step, mirroring
	// the registration-check rotation below (registrationCheckCursor). Without
	// rotation, a single worker whose deregisterRunner call keeps returning a
	// persistent error would consume this Step's entire per-step retry budget
	// every Step forever -- it always sorts first in plan.Remove and the cap
	// check above is reached (and the budget spent) before any later entry is
	// ever tried -- starving every worker behind it from retiring at all.
	// Rotating the starting point guarantees every worker eventually reaches
	// the front of the budget, as long as new excess inventory does not
	// outpace the cap indefinitely, the same assumption
	// registrationCheckCursor already relies on.
	removalOrder := make([]int, len(plan.Remove))
	if n := len(removalOrder); n > 0 {
		start := int(r.retirementCursor % uint64(n))
		for i := range removalOrder {
			removalOrder[i] = (start + i) % n
		}
	}

	seenRemoval := map[string]struct{}{}
	for _, removeIndex := range removalOrder {
		worker := plan.Remove[removeIndex]
		if _, duplicate := seenRemoval[worker.ID]; duplicate {
			continue
		}
		seenRemoval[worker.ID] = struct{}{}
		if worker.State == model.WorkerBusy {
			record("busy-worker-removal-refused", "controller policy refused to remove a busy worker", worker.PoolID, false, nil)
			continue
		}
		if worker.State == model.WorkerUnregistered {
			removed, removeErr := r.deps.Workers.RemoveIfIdle(ctx, worker.ID)
			if removeErr != nil {
				record("unregistered-worker-remove-error", "unregistered idle worker removal failed", worker.PoolID, true, removeErr)
				continue
			}
			if !removed {
				record("unregistered-worker-became-busy", "worker acquired work during final removal verification and was preserved", worker.PoolID, false, nil)
				continue
			}
			r.writeLog(ctx, LogEvent{At: now, Code: "unregistered-worker-removed", Message: "removed idle container after GitHub deleted its one-job registration", PoolID: worker.PoolID, WorkerID: worker.ID})
			continue
		}
		requiredCapacity, configuredPool := plan.AdvertisedCapacity[worker.PoolID]
		if worker.State != model.WorkerExited && !configuredPool {
			record("worker-retirement-pool-unknown", "automatic retirement was refused because the worker pool is not configured and cannot be quiesced", worker.PoolID, false, nil)
			continue
		}
		if worker.State != model.WorkerExited && !jobStateKnown {
			record("worker-retirement-job-state-unknown", "automatic retirement was refused because durable job state is unavailable", worker.PoolID, true, nil)
			continue
		}
		if configuredPool && !capacityAcknowledged[worker.PoolID] {
			record("worker-removal-capacity-unconfirmed", "idle worker removal was deferred because the listener did not acknowledge the capacity update", worker.PoolID, true, nil)
			continue
		}
		if configuredPool && acknowledgedCapacity[worker.PoolID] != requiredCapacity {
			record("worker-removal-capacity-stale", "idle worker removal was deferred until the listener acknowledges the current planned capacity", worker.PoolID, true, nil)
			continue
		}
		if worker.State != model.WorkerExited && configuredPool && requiredCapacity != 0 {
			record("worker-retirement-quiescence-required", "automatic retirement was deferred because the pool still advertises assignment headroom", worker.PoolID, false, nil)
			continue
		}
		if configuredPool && requiredCapacity == 0 && !zeroAcknowledged[worker.PoolID] {
			note("worker-removal-zero-unconfirmed", "idle worker removal was deferred until the listener acknowledges zero capacity", worker.PoolID)
			continue
		}
		pool := findPool(pools, worker.PoolID)
		if worker.State != model.WorkerExited && (pool.TotalAssignedJobs != 0 || zeroConfirmations[worker.PoolID] < 2) {
			note("worker-retirement-zero-stability-unconfirmed", "automatic retirement was deferred until two capacity-zero polls report no assigned jobs", worker.PoolID)
			continue
		}
		if worker.State != model.WorkerExited {
			if worker.RunnerID <= 0 {
				record("worker-retirement-runner-id-missing", "automatic retirement was refused because the managed worker has no persisted GitHub runner ID", worker.PoolID, false, nil)
				continue
			}
			if retirementDeregistrations >= retirementDeregistrationCap {
				note("worker-retirement-deferred-step-budget", "idle worker retirement was deferred to a later reconcile step to stay within this step's deregistration retry budget", worker.PoolID)
				continue
			}
			retirementDeregistrations++
			if removeRunnerErr := r.deregisterRunner(ctx, worker.PoolID, worker.RunnerID); removeRunnerErr != nil {
				record("runner-deregistration-error", safeScaleSetMessage("deregister quiesced runner", removeRunnerErr), worker.PoolID, scaleset.Retryable(removeRunnerErr), removeRunnerErr)
				continue
			}
		}
		removed, removeErr := r.deps.Workers.RemoveIfIdle(ctx, worker.ID)
		if removeErr != nil {
			record("worker-remove-error", "idle worker removal failed", worker.PoolID, true, removeErr)
			continue
		}
		if !removed {
			record("worker-became-busy", "worker acquired work during drain and was preserved", worker.PoolID, false, nil)
		}
	}
	r.retirementCursor += uint64(retirementDeregistrations)

	var reservedMemory uint64
	for _, decision := range plan.Start {
		identity, ok := identities[decision.PoolID]
		if !ok {
			continue
		}
		limits, configured := r.config.WorkerForTarget(decision.PoolID)
		if !configured {
			record("worker-profile-missing", "planned worker target has no configured resource profile", decision.PoolID, false, nil)
			continue
		}
		for count := 0; count < decision.Count; count++ {
			if err := ctx.Err(); err != nil {
				operationErrors = append(operationErrors, err)
				break
			}
			allowed, safetyChanged, admissionErr := r.freshStartAllowed(ctx, decision.PoolID, pools, previous, desired, power, resources, desktop, reservedMemory)
			if admissionErr != nil || safetyChanged {
				cancel(errReconcileInputsChanged)
				return ReconcileResult{}, errReconcileInputsChanged
			}
			if !allowed {
				break
			}
			name := r.nextRunnerName(decision.PoolID, now)
			resourceTier := r.resourceTier(decision.PoolID)
			registrationStartedAt := time.Now()
			jit, jitErr := RetryValue(ctx, r.deps.Clock, r.backoffPolicy(), scaleset.Retryable, func(callCtx context.Context) (scaleset.JITConfig, error) {
				return r.deps.ScaleSets.CreateJITConfig(callCtx, identity, name)
			})
			r.deps.Telemetry.WorkerRegistered(ctx, decision.PoolID, resourceTier, time.Since(registrationStartedAt), telemetry.ClassifyWorkerStart(jitErr, false))
			if jitErr != nil {
				record("jit-config-error", safeScaleSetMessage("create JIT configuration", jitErr), decision.PoolID, scaleset.Retryable(jitErr), jitErr)
				break
			}
			// JIT creation is a network operation. Re-read every safety and
			// admission input again immediately before the irreversible container
			// start so a stale pre-JIT snapshot cannot admit work.
			allowed, safetyChanged, admissionErr = r.freshStartAllowed(ctx, decision.PoolID, pools, previous, desired, power, resources, desktop, reservedMemory)
			if admissionErr != nil || safetyChanged {
				cleanupContext, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), r.config.GitHub.RequestTimeout.Duration)
				cleanupErr := r.deregisterRunner(cleanupContext, decision.PoolID, jit.RunnerID())
				cleanupCancel()
				if cleanupErr != nil {
					r.writeLog(ctx, LogEvent{At: r.deps.Clock.Now().UTC(), Code: "unused-jit-runner-cleanup-error", Message: safeScaleSetMessage("deregister unused JIT runner", cleanupErr), PoolID: decision.PoolID})
				}
				cancel(errReconcileInputsChanged)
				return ReconcileResult{}, errReconcileInputsChanged
			}
			if !allowed {
				cleanupContext, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), r.config.GitHub.RequestTimeout.Duration)
				cleanupErr := r.deregisterRunner(cleanupContext, decision.PoolID, jit.RunnerID())
				cleanupCancel()
				if cleanupErr != nil {
					record("unused-jit-runner-cleanup-error", safeScaleSetMessage("deregister unused JIT runner", cleanupErr), decision.PoolID, scaleset.Retryable(cleanupErr), cleanupErr)
				}
				break
			}
			_, startErr := r.deps.Workers.Start(ctx, StartWorkerRequest{
				PoolID: decision.PoolID, Name: name, ResourceTier: resourceTier, JITConfig: jit, Limits: limits,
			})
			if startErr != nil {
				// Start is intentionally not retried: it may have created a running
				// container before returning an error. The next inventory reconciles it.
				record("worker-start-error", "one-job worker start failed; inventory will reconcile before retry", decision.PoolID, true, startErr)
				if !RunnerStartMayBeActive(startErr) {
					cleanupContext, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), r.config.GitHub.RequestTimeout.Duration)
					cleanupErr := r.deregisterRunner(cleanupContext, decision.PoolID, jit.RunnerID())
					cleanupCancel()
					if cleanupErr != nil {
						record("failed-start-runner-cleanup-error", safeScaleSetMessage("deregister runner after pre-start failure", cleanupErr), decision.PoolID, scaleset.Retryable(cleanupErr), cleanupErr)
					}
				}
				break
			}
			reservedMemory = saturatingAddUint64(reservedMemory, uint64(limits.Memory))
		}
	}

	if plan.StopDesktop || plan.ShutdownWSL {
		if !allTargetsStablyZero(r.config.GitHub.Targets, zeroAcknowledged, zeroConfirmations) {
			note("gaming-zero-capacity-unconfirmed", "Docker and WSL shutdown was deferred until every listener has two zero-capacity, zero-assignment confirmations", "")
			plan.StopDesktop = false
			plan.ShutdownWSL = false
		}
	}
	if plan.StopDesktop || plan.ShutdownWSL {
		var currentWorkers []model.Worker
		var inventoryErr error
		if mayHaveManagedWorkers(desktop, desktopStatusKnown) {
			currentWorkers, inventoryErr = r.deps.Workers.List(ctx)
		}
		if inventoryErr != nil {
			record("gaming-preflight-error", "gaming shutdown was deferred because worker status could not be verified", "", true, inventoryErr)
		} else if enriched, lookupErr := r.enrichWorkerJobs(ctx, currentWorkers); lookupErr != nil {
			record("gaming-job-index-error", "gaming shutdown was deferred because durable job lifecycle state could not be verified", "", true, lookupErr)
		} else if containsActiveWorker(enriched) {
			record("gaming-active-race", "gaming shutdown was deferred because a managed worker is still active", "", false, nil)
		} else {
			if plan.StopDesktop {
				if stopErr := r.deps.Desktop.Stop(ctx, r.config.DockerDesktop.StopTimeout.Duration); stopErr != nil {
					record("desktop-stop-error", "Docker Desktop did not stop within the configured policy", "", true, stopErr)
				}
			}
			if plan.ShutdownWSL {
				if shutdownErr := r.deps.Desktop.ShutdownAllWSL(ctx); shutdownErr != nil {
					record("wsl-shutdown-error", "one or more WSL distributions remain running", "", true, shutdownErr)
				}
			}
		}
	}

	if latest, statusErr := r.deps.Desktop.Status(ctx); statusErr == nil {
		desktop = latest
		desktopStatusKnown = true
	} else {
		desktopStatusKnown = false
		record("desktop-final-status-error", "final Docker Desktop status failed", "", true, statusErr)
	}
	if mayHaveManagedWorkers(desktop, desktopStatusKnown) {
		if latest, listErr := r.deps.Workers.List(ctx); listErr == nil {
			if enriched, lookupErr := r.enrichWorkerJobs(ctx, latest); lookupErr != nil {
				record("job-index-final-error", "final durable job lifecycle state could not be read", "", true, lookupErr)
			} else {
				workers = enriched
			}
		} else {
			record("worker-final-inventory-error", "final managed-worker inventory failed", "", true, listErr)
		}
	} else {
		workers = nil
	}

	postPlan := BuildPlan(PlanInput{
		Config: r.config, Desired: desired, Previous: previous, Pools: pools,
		Workers: workers, Resources: resources, Power: power, Desktop: desktop,
		EngineMemoryTotalBytes: r.engineMemoryTotal, Now: now,
	})
	observedPools := make([]model.PoolObservation, 0, len(r.config.GitHub.Targets))
	for _, target := range r.config.GitHub.Targets {
		pool := findPool(pools, target.ID)
		priorPool := cadencePools[target.ID]
		actualCapacity := priorPool.MaxCapacity
		if capacityAcknowledged[target.ID] {
			actualCapacity = acknowledgedCapacity[target.ID]
		}
		updatedAt := r.poolAcknowledgementTransitionAt(
			target.ID, priorPool, pool.Identity.ScaleSetID, pool.Identity.ListenerID,
			actualCapacity, pollPlan.AdvertisedCapacity[target.ID], capacityAcknowledged[target.ID], now,
		)
		observedPools = append(observedPools, model.PoolObservation{
			ID: target.ID, ScaleSetID: pool.Identity.ScaleSetID, ListenerID: pool.Identity.ListenerID,
			TotalAssignedJobs: pool.TotalAssignedJobs, MaxCapacity: actualCapacity, CapacityAcknowledged: capacityAcknowledged[target.ID],
			ZeroCapacityConfirmations: zeroConfirmations[target.ID],
			DrainServiceCapacity:      pool.DrainServiceCapacity,
			DesiredWorkers:            postPlan.DesiredWorkers[target.ID], UpdatedAt: updatedAt,
		})
	}
	// The memory clamp was previously invisible: capacity silently advertised
	// below pool max whenever the memory term bound. A log line (not a
	// problem) keeps the routine legacy-basis clamp from flipping the fleet
	// phase while still leaving a queryable trail.
	for _, target := range r.config.GitHub.Targets {
		if postPlan.MemoryClamped[target.ID] {
			note("memory-clamped-capacity", "the memory term or host-floor backstop clamped worker starts or advertised capacity below host and pool limits", target.ID)
		}
	}
	problems := append([]model.Problem(nil), postPlan.Problems...)
	problems = append(problems, operationProblems...)
	var drainStartedAt *time.Time
	if postPlan.Phase == model.PhaseDraining {
		drainStartedAt = previous.DrainStartedAt
		if drainStartedAt == nil || now.Before(*drainStartedAt) {
			value := now
			drainStartedAt = &value
		}
		if now.Sub(*drainStartedAt) >= r.config.Drain.WarningAfter.Duration {
			problems = append(problems, problem(now, "drain-warning", "drain is taking longer than the configured warning threshold; active work will continue", "", false))
		}
	}
	phase := postPlan.Phase
	if len(operationProblems) > 0 {
		phase = model.PhaseDegraded
	}
	observed := model.ObservedState{
		SchemaVersion: 1, Phase: phase, HeartbeatAt: r.deps.Clock.Now().UTC(), DrainStartedAt: drainStartedAt, Version: r.version,
		Pools: observedPools, Workers: append([]model.Worker(nil), workers...), Resources: resources,
		Power: power, Desktop: desktop, ResourceGate: postPlan.ResourceGate, PowerGate: postPlan.PowerGate,
		Problems: problems,
	}
	if saveErr := r.deps.State.SaveObserved(ctx, observed); saveErr != nil {
		operationErrors = append(operationErrors, fmt.Errorf("save observed state: %w", saveErr))
	}
	return ReconcileResult{
		Observed: observed, Plan: postPlan,
		CheckpointAge: checkpointAge, CheckpointAgeValid: checkpointAgeValid,
	}, errors.Join(operationErrors...)
}

// probeEngineMemory maintains the cached engine VM MemTotal that cross-checks
// a configured worker memory budget. It probes at most once per VM lifecycle:
// only after Docker Desktop and its engine are confirmed up (the VM may not
// exist at process start), and again after any down observation, because
// desktop teardown (gaming mode) recycles the WSL2 VM and a stale probe must
// not vouch for the next VM's size. An UNKNOWN status (a failed status query)
// keeps the cache: the VM was never observed down, and discarding the probe
// would leave an oversized budget unverified if the follow-up re-probe also
// failed. A probe failure is a warning, not a gate: the configured budget is
// used unverified.
func (r *Reconciler) probeEngineMemory(ctx context.Context, desktopStatusKnown bool, desktop model.DesktopStatus, note func(code, message, poolID string)) {
	if r.config.Resources.WorkerMemoryBudget == 0 {
		return
	}
	if !desktopStatusKnown {
		return
	}
	if !desktop.DesktopRunning || !desktop.EngineReachable {
		r.engineMemoryTotal = 0
		return
	}
	if r.engineMemoryTotal != 0 {
		return
	}
	total, err := r.deps.EngineMemory.EngineMemoryTotal(ctx)
	if err != nil || total == 0 {
		note("engine-memory-probe-error", "engine VM memory could not be probed; the configured workerMemoryBudget is used unverified", "")
		return
	}
	r.engineMemoryTotal = total
}

func (r *Reconciler) resourceTier(poolID string) string {
	for _, target := range r.config.GitHub.Targets {
		if target.ID == poolID {
			if target.Resources.Worker != nil {
				return "target_override"
			}
			return "default"
		}
	}
	return "unknown"
}

func (r *Reconciler) ensure(ctx context.Context, target config.Target, previous *scaleset.Identity) (scaleset.Identity, error) {
	definition := scaleset.Definition{
		TargetID: target.ID, URL: target.URL, Scope: string(target.Scope), ClientID: target.ClientID, InstallationID: target.InstallationID, SecretID: target.SecretID,
		RunnerGroup: target.RunnerGroup, ScaleSetName: target.ScaleSetName, Labels: append([]string(nil), target.Labels...),
	}
	return RetryValue(ctx, r.deps.Clock, r.backoffPolicy(), scaleset.Retryable, func(callCtx context.Context) (scaleset.Identity, error) {
		return r.deps.ScaleSets.Ensure(callCtx, definition, previous)
	})
}

func (r *Reconciler) statistics(ctx context.Context, target config.Target, identity scaleset.Identity, maxCapacity int) (scaleset.Statistics, scaleset.Identity, error) {
	stats, err := RetryValue(ctx, r.deps.Clock, r.backoffPolicy(), scaleset.Retryable, func(callCtx context.Context) (scaleset.Statistics, error) {
		return r.deps.ScaleSets.Statistics(callCtx, identity, maxCapacity)
	})
	if err == nil {
		return stats, identity, nil
	}
	if !scaleset.IsKind(err, scaleset.ErrorNotFound) {
		return scaleset.Statistics{}, identity, err
	}
	// The persisted scale set was deleted externally. Recreate this host's own
	// identity, then retry the statistics call once through normal backoff.
	recreated, ensureErr := r.ensure(ctx, target, nil)
	if ensureErr != nil {
		return scaleset.Statistics{}, identity, errors.Join(err, ensureErr)
	}
	stats, statsErr := RetryValue(ctx, r.deps.Clock, r.backoffPolicy(), scaleset.Retryable, func(callCtx context.Context) (scaleset.Statistics, error) {
		return r.deps.ScaleSets.Statistics(callCtx, recreated, maxCapacity)
	})
	return stats, recreated, statsErr
}

func (r *Reconciler) deregisterRunner(ctx context.Context, poolID string, runnerID int64) error {
	if poolID == "" || runnerID <= 0 {
		return errors.New("positive GitHub runner ID and pool ID are required for deregistration")
	}
	_, err := RetryValue(ctx, r.deps.Clock, r.backoffPolicy(), scaleset.Retryable, func(callContext context.Context) (struct{}, error) {
		err := r.deps.ScaleSets.RemoveRunner(callContext, poolID, runnerID)
		if scaleset.IsKind(err, scaleset.ErrorNotFound) {
			err = nil
		}
		return struct{}{}, err
	})
	return err
}

func (r *Reconciler) runnerRegistered(ctx context.Context, poolID string, runnerID int64, runnerName string) (bool, error) {
	return RetryValue(ctx, r.deps.Clock, r.backoffPolicy(), scaleset.Retryable, func(callContext context.Context) (bool, error) {
		return r.deps.ScaleSets.RunnerRegistered(callContext, poolID, runnerID, runnerName)
	})
}

func (r *Reconciler) target(id string) config.Target {
	for _, target := range r.config.GitHub.Targets {
		if target.ID == id {
			return target
		}
	}
	return config.Target{ID: id}
}

func (r *Reconciler) nextRunnerName(_ string, now time.Time) string {
	r.stateMu.Lock()
	r.sequence++
	sequence := r.sequence
	r.stateMu.Unlock()
	suffix := "-" + strconv.FormatInt(now.UnixNano(), 36) + "-" + strconv.FormatUint(sequence, 36)
	base := r.config.Host.RunnerNamePrefix
	const maximum = 64
	if len(base)+len(suffix) > maximum {
		base = strings.TrimRight(base[:maximum-len(suffix)], "-._")
	}
	return base + suffix
}

func (r *Reconciler) backoffPolicy() BackoffPolicy {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.backoff
}

func (r *Reconciler) safetyWatchInterval() time.Duration {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.watchInterval
}

func (r *Reconciler) rememberedDrainCapacity(poolID string) int {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.drainCapacity[poolID]
}

func (r *Reconciler) rememberDrainCapacity(poolID string, capacity int) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if capacity <= 0 {
		delete(r.drainCapacity, poolID)
		return
	}
	r.drainCapacity[poolID] = capacity
}

func (r *Reconciler) watchSafetyInputs(ctx context.Context, desired model.DesiredState, power model.PowerSnapshot, cancel context.CancelCauseFunc, forcedZero bool) {
	ticker := time.NewTicker(r.safetyWatchInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !forcedZero {
				latestDesired, err := r.deps.State.LoadDesired(ctx)
				if errors.Is(err, statepkg.ErrNotFound) {
					latestDesired = model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled}
					err = nil
				}
				if err != nil || !sameAdmissionIntent(desired, latestDesired) {
					cancel(errReconcileInputsChanged)
					return
				}
			}
			if !forcedZero && r.config.Power.Policy == config.PowerACOnly {
				latestPower, err := r.deps.Power.Snapshot(ctx)
				if err != nil || latestPower.ACConnected != power.ACConnected {
					cancel(errReconcileInputsChanged)
					return
				}
			}
		}
	}
}

func (r *Reconciler) freshStartAllowed(
	ctx context.Context,
	poolID string,
	pools []PoolSnapshot,
	previous model.ObservedState,
	baselineDesired model.DesiredState,
	baselinePower model.PowerSnapshot,
	baselineResources model.ResourceSnapshot,
	baselineDesktop model.DesktopStatus,
	reservedMemory uint64,
) (allowed bool, safetyChanged bool, err error) {
	desired, err := r.deps.State.LoadDesired(ctx)
	if errors.Is(err, statepkg.ErrNotFound) {
		desired = model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled}
		err = nil
	}
	if err != nil {
		return false, true, err
	}
	if r.isShuttingDown() {
		desired.Mode = model.ModeDisabled
		desired.TemporaryCapacityOverride = nil
	}
	if !sameAdmissionIntent(baselineDesired, desired) {
		return false, true, nil
	}

	power, err := r.deps.Power.Snapshot(ctx)
	if err != nil {
		return false, true, err
	}
	if r.config.Power.Policy == config.PowerACOnly && power.ACConnected != baselinePower.ACConnected {
		return false, true, nil
	}
	resources, err := r.deps.Resources.Snapshot(ctx)
	if err != nil {
		return false, true, err
	}
	desktop, err := r.deps.Desktop.Status(ctx)
	if err != nil {
		return false, true, err
	}
	workers, err := r.deps.Workers.List(ctx)
	if err != nil {
		return false, true, err
	}
	enriched, lookupErr := r.enrichWorkerJobs(ctx, workers)
	if lookupErr != nil {
		return false, true, lookupErr
	}
	workers = enriched

	// A host monitor may not reflect containers started earlier in this Step.
	// Under the legacy host-headroom basis, reserve the exact sum of their
	// target profiles against every fresh observation so a stale snapshot
	// cannot over-admit mixed worker sizes. Under the static budget basis the
	// fresh worker list above already charges them against the budget, and
	// the host reading only feeds the binary floor - synthetically deflating
	// it would let worker growth alone trip the floor, the exact coupling the
	// budget basis exists to remove (stress-test C1).
	if r.config.Resources.WorkerMemoryBudget == 0 {
		resources.AvailableMemoryBytes = availableAfterMemoryReservation(resources.AvailableMemoryBytes, reservedMemory)
	}

	now := r.deps.Clock.Now().UTC()
	plan := BuildPlan(PlanInput{
		Config: r.config, Desired: desired, Previous: previous, Pools: pools,
		Workers: workers, Resources: resources, Power: power, Desktop: desktop,
		EngineMemoryTotalBytes: r.engineMemoryTotal, Now: now,
	})
	for _, decision := range plan.Start {
		if decision.PoolID == poolID && decision.Count > 0 {
			return true, false, nil
		}
	}
	changed := resources != baselineResources || desktop != baselineDesktop
	return false, changed && reservedMemory == 0, nil
}

func saturatingAddUint64(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}

func availableAfterMemoryReservation(available, reserved uint64) uint64 {
	if reserved >= available {
		return 0
	}
	return available - reserved
}

const diagnosticLogWriteTimeout = 2 * time.Second

func (r *Reconciler) writeLog(ctx context.Context, event LogEvent) {
	writeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), diagnosticLogWriteTimeout)
	defer cancel()
	_ = r.deps.Logs.Write(writeContext, event)
}

func sameAdmissionIntent(left, right model.DesiredState) bool {
	if left.SchemaVersion != right.SchemaVersion || left.Mode != right.Mode {
		return false
	}
	if left.TemporaryCapacityOverride == nil || right.TemporaryCapacityOverride == nil {
		return left.TemporaryCapacityOverride == nil && right.TemporaryCapacityOverride == nil
	}
	return *left.TemporaryCapacityOverride == *right.TemporaryCapacityOverride
}

// pollSuperseded reports whether a listener poll error is the controller's own
// designed supersession rather than a GitHub or transport failure.
// watchPollCadence cancels the open poll with errReconcileInputsChanged when
// reconciliation safety inputs change; the in-flight poll then returns
// context.Canceled and Step reruns. Only the result's own error is consulted:
// once the cadence watcher cancels, the step cancellation cause is set for
// every queued result, and a genuine *scaleset.Error from another pool must
// still surface its log line even though the step result is discarded.
func pollSuperseded(err error) bool {
	return errors.Is(err, context.Canceled)
}

func safeScaleSetMessage(operation string, err error) string {
	var typed *scaleset.Error
	if errors.As(err, &typed) {
		if typed.StatusCode > 0 {
			return fmt.Sprintf("scale-set %s failed (%s, HTTP %d)", operation, typed.Kind, typed.StatusCode)
		}
		return fmt.Sprintf("scale-set %s failed (%s)", operation, typed.Kind)
	}
	return fmt.Sprintf("scale-set %s failed", operation)
}

func findPool(pools []PoolSnapshot, id string) PoolSnapshot {
	for _, pool := range pools {
		if pool.TargetID == id {
			return pool
		}
	}
	return PoolSnapshot{TargetID: id}
}

func containsActiveWorker(workers []model.Worker) bool {
	for _, worker := range workers {
		if worker.Active() {
			return true
		}
	}
	return false
}

func allTargetsStablyZero(targets []config.Target, acknowledged map[string]bool, confirmations map[string]int) bool {
	for _, target := range targets {
		if !acknowledged[target.ID] || confirmations[target.ID] < 2 {
			return false
		}
	}
	return true
}

// mayHaveManagedWorkers distinguishes the authoritative Desktop-stopped state
// from an ambiguous engine failure. When both Desktop and its engine are known
// stopped, no managed container can be active and querying Docker is expected
// to fail. Every unknown or contradictory state still requires inventory and
// therefore fails closed if the runtime cannot provide it.
func mayHaveManagedWorkers(desktop model.DesktopStatus, statusKnown bool) bool {
	return !statusKnown || desktop.DesktopRunning || desktop.EngineReachable
}

func (r *Reconciler) enrichWorkerJobs(ctx context.Context, workers []model.Worker) ([]model.Worker, error) {
	result := append([]model.Worker(nil), workers...)
	for index := range result {
		jobID, found, err := r.deps.Jobs.ActiveJob(ctx, result[index].PoolID, result[index].Name)
		if err != nil {
			return nil, err
		}
		if found {
			result[index].JobID = jobID
			result[index].State = model.WorkerBusy
		}
	}
	return result, nil
}
