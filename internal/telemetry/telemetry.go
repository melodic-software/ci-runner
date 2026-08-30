// Package telemetry provides the controller's low-cardinality OpenTelemetry
// contract. Callers pass only aggregate fleet state; runner names, job IDs,
// container IDs, credentials, and GitHub request payloads never cross it.
package telemetry

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const instrumentationName = "github.com/melodic-software/ci-runner/internal/telemetry"

var workerStates = [...]string{"starting", "idle", "busy", "unregistered", "exited"}

// Recorder is deliberately infallible. Telemetry must never participate in
// worker admission, draining, finalization, or controller shutdown decisions.
type Recorder interface {
	BeginReconcile(context.Context) (context.Context, func(ReconcileSnapshot, error))
	RecordCapacityCheckpoint(context.Context, time.Time, []CapacityCheckpointPool)
	WorkerRegistered(context.Context, string, string, time.Duration, WorkerStartOutcome)
	WorkerStarted(context.Context, string, string, time.Duration, WorkerStartOutcome)
	WorkerFinalized(context.Context, string, WorkerFinalization)
	ObserveJobStarted(context.Context, string, time.Duration)
	ObserveJobCompleted(context.Context, string, string, bool)
}

// CapacityCheckpointPool carries durable poll-cadence acknowledgement state
// without requiring a completed reconcile snapshot.
type CapacityCheckpointPool struct {
	ID                             string
	CapacityAcknowledged           bool
	AcknowledgementPendingAge      time.Duration
	AcknowledgementPendingAgeValid bool
}

type ReconcilePool struct {
	ID                             string
	Advertised                     int
	Assigned                       int
	Desired                        int
	AffordableWorkers              int
	CapacityAcknowledged           bool
	AcknowledgementPendingAge      time.Duration
	AcknowledgementPendingAgeValid bool
}

type ReconcileWorker struct {
	PoolID string
	State  string
}

type ReconcileSnapshot struct {
	Valid                bool
	Phase                string
	Pools                []ReconcilePool
	Workers              []ReconcileWorker
	CPUPercent           float64
	AvailableMemoryBytes uint64
	MemoryHeadroomBytes  uint64
	ResourceGateBlocked  bool
	PowerGateBlocked     bool
	CheckpointAge        time.Duration
	CheckpointAgeValid   bool
}

type WorkerStartOutcome string

const (
	WorkerStartSucceeded WorkerStartOutcome = "succeeded"
	WorkerStartFailed    WorkerStartOutcome = "failed"
	WorkerStartAmbiguous WorkerStartOutcome = "ambiguous"
	WorkerStartCanceled  WorkerStartOutcome = "canceled"
)

func ClassifyWorkerStart(err error, mayBeActive bool) WorkerStartOutcome {
	if err == nil {
		return WorkerStartSucceeded
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return WorkerStartCanceled
	}
	if mayBeActive {
		return WorkerStartAmbiguous
	}
	return WorkerStartFailed
}

type WorkerFinalization struct {
	ExitCode               int64
	ExitObserved           bool
	Err                    error
	ControllerShutdown     bool
	ResourceTier           string
	ResourceEvidence       *WorkerResourceEvidence
	RecordResourceEvidence bool
	Duration               time.Duration
}

type WorkerResourceEvidence struct {
	Status                   string
	Missing                  []string
	MemoryPeakBytes          uint64
	MemorySwapPeakBytes      uint64
	OOMEvents                uint64
	OOMKillEvents            uint64
	CPUPeriods               uint64
	CPUThrottledPeriods      uint64
	CPUThrottledMicroseconds uint64
	PIDsPeak                 uint64
	IOReadBytes              uint64
	IOWriteBytes             uint64
}

type WorkerFinalizationOutcome string

const (
	WorkerFinalizationCompleted    WorkerFinalizationOutcome = "completed"
	WorkerFinalizationWorkerError  WorkerFinalizationOutcome = "worker_error"
	WorkerFinalizationRuntimeError WorkerFinalizationOutcome = "runtime_error"
	WorkerFinalizationCanceled     WorkerFinalizationOutcome = "canceled"
	WorkerFinalizationUnknown      WorkerFinalizationOutcome = "unknown"
)

// ClassifyWorkerFinalization uses the container lifecycle, not log text. The
// one-shot GitHub runner can log TaskCanceledException while shutting down its
// broker poll after a successful job; a zero container exit remains completed.
func ClassifyWorkerFinalization(value WorkerFinalization) WorkerFinalizationOutcome {
	if value.ControllerShutdown {
		return WorkerFinalizationCanceled
	}
	if value.Err != nil {
		return WorkerFinalizationRuntimeError
	}
	if !value.ExitObserved {
		return WorkerFinalizationUnknown
	}
	if value.ExitCode == 0 {
		return WorkerFinalizationCompleted
	}
	return WorkerFinalizationWorkerError
}

type noopRecorder struct{}

func Noop() Recorder { return noopRecorder{} }

func (noopRecorder) BeginReconcile(ctx context.Context) (context.Context, func(ReconcileSnapshot, error)) {
	return ctx, func(ReconcileSnapshot, error) {}
}
func (noopRecorder) RecordCapacityCheckpoint(context.Context, time.Time, []CapacityCheckpointPool) {}
func (noopRecorder) WorkerRegistered(context.Context, string, string, time.Duration, WorkerStartOutcome) {
}
func (noopRecorder) WorkerStarted(context.Context, string, string, time.Duration, WorkerStartOutcome) {
}
func (noopRecorder) WorkerFinalized(context.Context, string, WorkerFinalization) {}
func (noopRecorder) ObserveJobStarted(context.Context, string, time.Duration)    {}
func (noopRecorder) ObserveJobCompleted(context.Context, string, string, bool)   {}

type recorder struct {
	tracer trace.Tracer

	reconcileDuration         metric.Float64Histogram
	reconcileErrors           metric.Int64Counter
	checkpointAge             metric.Float64Gauge
	advertised                metric.Int64Gauge
	capacityAcknowledged      metric.Int64Gauge
	acknowledgementPendingAge metric.Float64Gauge
	assigned                  metric.Int64Gauge
	desired                   metric.Int64Gauge
	workers                   metric.Int64Gauge
	activeJobs                metric.Int64Gauge
	inventoryWorkers          metric.Int64Gauge
	assignmentGap             metric.Int64Gauge
	transientLag              metric.Int64Gauge
	cpuPercent                metric.Float64Gauge
	availableMemory           metric.Int64Gauge
	memoryHeadroom            metric.Int64Gauge
	memoryAffordable          metric.Int64Gauge
	resourceGate              metric.Int64Gauge
	powerGate                 metric.Int64Gauge
	workerStarts              metric.Int64Counter
	workerStartTime           metric.Float64Histogram
	workerRegisters           metric.Int64Counter
	workerRegisterTime        metric.Float64Histogram
	workerFinalizes           metric.Int64Counter
	workerFinalizeTime        metric.Float64Histogram
	jobsStarted               metric.Int64Counter
	jobStartLag               metric.Float64Histogram
	lifecycleEventTime        metric.Float64Gauge
	jobsCompleted             metric.Int64Counter
	cancellations             metric.Int64Counter
	resourceEvidence          metric.Int64Counter
	memoryPeak                metric.Int64Histogram
	memorySwapPeak            metric.Int64Histogram
	oomEvents                 metric.Int64Counter
	oomKillEvents             metric.Int64Counter
	cpuPeriods                metric.Int64Histogram
	cpuThrottled              metric.Int64Histogram
	cpuThrottledTime          metric.Float64Histogram
	pidsPeak                  metric.Int64Histogram
	ioRead                    metric.Int64Histogram
	ioWrite                   metric.Int64Histogram
}

func newRecorder(tracerProvider trace.TracerProvider, meterProvider metric.MeterProvider) (*recorder, error) {
	meter := meterProvider.Meter(instrumentationName)
	r := &recorder{tracer: tracerProvider.Tracer(instrumentationName)}
	var err error
	float64Histogram := func(name, unit, description string) (instrument metric.Float64Histogram) {
		if err == nil {
			instrument, err = meter.Float64Histogram(name, metric.WithUnit(unit), metric.WithDescription(description))
		}
		return instrument
	}
	float64Gauge := func(name, unit, description string) (instrument metric.Float64Gauge) {
		if err == nil {
			instrument, err = meter.Float64Gauge(name, metric.WithUnit(unit), metric.WithDescription(description))
		}
		return instrument
	}
	int64Counter := func(name, unit, description string) (instrument metric.Int64Counter) {
		if err == nil {
			instrument, err = meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(description))
		}
		return instrument
	}
	int64Gauge := func(name, unit, description string) (instrument metric.Int64Gauge) {
		if err == nil {
			instrument, err = meter.Int64Gauge(name, metric.WithUnit(unit), metric.WithDescription(description))
		}
		return instrument
	}
	int64Histogram := func(name, unit, description string) (instrument metric.Int64Histogram) {
		if err == nil {
			instrument, err = meter.Int64Histogram(name, metric.WithUnit(unit), metric.WithDescription(description))
		}
		return instrument
	}
	r.reconcileDuration = float64Histogram("ci_runner.controller.reconcile.duration", "s", "Controller reconciliation duration.")
	r.reconcileErrors = int64Counter("ci_runner.controller.reconcile.errors", "{error}", "Unexpected reconciliation errors.")
	r.checkpointAge = float64Gauge("ci_runner.controller.observed.checkpoint.age", "s", "Age of the prior durable observed checkpoint when reconciliation began.")
	r.advertised = int64Gauge("ci_runner.capacity.advertised", "{worker}", "Capacity acknowledged to GitHub by target pool.")
	r.capacityAcknowledged = int64Gauge("ci_runner.capacity.acknowledged", "1", "Whether the latest target capacity is acknowledged by the GitHub listener.")
	r.acknowledgementPendingAge = float64Gauge("ci_runner.capacity.acknowledgement.pending.age", "s", "Age of the current unacknowledged capacity transition; omitted when acknowledged or unavailable.")
	r.assigned = int64Gauge("ci_runner.capacity.assigned", "{job}", "Authoritative assigned jobs by target pool.")
	r.desired = int64Gauge("ci_runner.capacity.desired", "{worker}", "Desired workers by target pool.")
	r.workers = int64Gauge("ci_runner.workers", "{worker}", "Managed workers by target pool and bounded lifecycle state.")
	r.activeJobs = int64Gauge("ci_runner.jobs.active", "{job}", "Active jobs by target pool.")
	r.inventoryWorkers = int64Gauge("ci_runner.docker.inventory.workers", "{worker}", "Workers present in the reconciled Docker inventory by target pool.")
	r.assignmentGap = int64Gauge("ci_runner.accounting.assignment.gap", "{job}", "GitHub assigned jobs minus locally visible busy or starting workers, floored at zero.")
	r.transientLag = int64Gauge("ci_runner.accounting.transient_lag", "1", "Whether short-job timing creates transientAccountingLag for a target pool.")
	r.cpuPercent = float64Gauge("ci_runner.host.cpu.utilization", "%", "Host CPU utilization percentage.")
	r.availableMemory = int64Gauge("ci_runner.host.memory.available", "By", "Available host physical memory.")
	r.memoryHeadroom = int64Gauge("ci_runner.capacity.memory.headroom", "By", "Memory headroom left unspent by the last plan under the active basis (static worker budget, or legacy host headroom).")
	r.memoryAffordable = int64Gauge("ci_runner.capacity.memory.affordable", "{worker}", "Additional workers the remaining memory headroom funds at the pool's effective worker profile.")
	r.resourceGate = int64Gauge("ci_runner.gate.resource.blocked", "1", "Whether resource admission is blocked.")
	r.powerGate = int64Gauge("ci_runner.gate.power.blocked", "1", "Whether power policy blocks admission.")
	r.workerStarts = int64Counter("ci_runner.worker.starts", "{worker}", "Worker start attempts by bounded outcome.")
	r.workerStartTime = float64Histogram("ci_runner.worker.start.duration", "s", "Docker worker start duration by bounded outcome.")
	r.workerRegisters = int64Counter("ci_runner.worker.registrations", "{worker}", "GitHub JIT worker registrations by bounded outcome.")
	r.workerRegisterTime = float64Histogram("ci_runner.worker.registration.duration", "s", "GitHub JIT worker registration duration by bounded outcome.")
	r.workerFinalizes = int64Counter("ci_runner.worker.finalizations", "{worker}", "Worker container finalizations by bounded outcome.")
	r.workerFinalizeTime = float64Histogram("ci_runner.worker.finalization.duration", "s", "Worker artifact and container finalization duration by bounded outcome.")
	r.jobsStarted = int64Counter("ci_runner.jobs.started", "{job}", "Durably indexed GitHub job-start events.")
	r.jobStartLag = float64Histogram("ci_runner.jobs.start.visibility_lag", "s", "Runner assignment to durable job-start observation lag.")
	r.lifecycleEventTime = float64Gauge("ci_runner.worker.lifecycle.event.time", "s", "Unix timestamp of the latest bounded worker lifecycle event.")
	r.jobsCompleted = int64Counter("ci_runner.jobs.completed", "{job}", "GitHub job completion events by bounded result.")
	r.cancellations = int64Counter("ci_runner.cancellations", "{cancellation}", "Expected cancellations by bounded source and classification.")
	r.resourceEvidence = int64Counter("ci_runner.worker.resource.evidence", "{worker}", "Terminal worker resource evidence records by bounded outcome.")
	r.memoryPeak = int64Histogram("ci_runner.worker.memory.peak", "By", "Terminal cgroup-v2 memory.peak by worker.")
	r.memorySwapPeak = int64Histogram("ci_runner.worker.memory.swap.peak", "By", "Terminal cgroup-v2 memory.swap.peak by worker.")
	r.oomEvents = int64Counter("ci_runner.worker.memory.oom.events", "{event}", "Terminal cgroup-v2 memory.events oom count.")
	r.oomKillEvents = int64Counter("ci_runner.worker.memory.oom_kill.events", "{event}", "Terminal cgroup-v2 memory.events oom_kill count.")
	r.cpuPeriods = int64Histogram("ci_runner.worker.cpu.periods", "{period}", "Terminal cgroup-v2 cpu.stat period count by worker.")
	r.cpuThrottled = int64Histogram("ci_runner.worker.cpu.throttled.periods", "{period}", "Terminal cgroup-v2 cpu.stat throttled period count by worker.")
	r.cpuThrottledTime = float64Histogram("ci_runner.worker.cpu.throttled.duration", "s", "Terminal cgroup-v2 cpu.stat throttled duration by worker.")
	r.pidsPeak = int64Histogram("ci_runner.worker.pids.peak", "{process}", "Terminal cgroup-v2 pids.peak by worker.")
	r.ioRead = int64Histogram("ci_runner.worker.io.read", "By", "Terminal aggregate cgroup-v2 io.stat read bytes by worker.")
	r.ioWrite = int64Histogram("ci_runner.worker.io.write", "By", "Terminal aggregate cgroup-v2 io.stat write bytes by worker.")
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *recorder) BeginReconcile(ctx context.Context) (context.Context, func(ReconcileSnapshot, error)) {
	started := time.Now()
	ctx, span := r.tracer.Start(ctx, "controller.reconcile", trace.WithSpanKind(trace.SpanKindInternal))
	return ctx, func(snapshot ReconcileSnapshot, reconcileErr error) {
		result, unexpected := classifyReconcileResult(reconcileErr)
		attributes := []attribute.KeyValue{attribute.String("ci_runner.reconcile.result", result)}
		if snapshot.Phase != "" {
			attributes = append(attributes, attribute.String("ci_runner.phase", snapshot.Phase))
		}
		if snapshotHasTransientAccountingLag(snapshot) {
			attributes = append(attributes, attribute.String("ci_runner.accounting.classification", "transientAccountingLag"))
		}
		span.SetAttributes(attributes...)
		if unexpected {
			r.reconcileErrors.Add(ctx, 1)
			span.RecordError(reconcileErr)
			span.SetStatus(codes.Error, "reconciliation failed")
		} else if result == "canceled" {
			r.cancellations.Add(ctx, 1, metric.WithAttributes(
				attribute.String("ci_runner.cancellation.source", "controller"),
				attribute.String("ci_runner.cancellation.classification", "shutdown"),
			))
		}
		r.reconcileDuration.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(attribute.String("ci_runner.reconcile.result", result)))
		if snapshot.Valid {
			r.recordSnapshot(ctx, snapshot)
		}
		span.End()
	}
}

func classifyReconcileResult(err error) (string, bool) {
	if err == nil {
		return "succeeded", false
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", false
	}
	return "failed", true
}

func (r *recorder) RecordCapacityCheckpoint(ctx context.Context, heartbeatAt time.Time, pools []CapacityCheckpointPool) {
	r.recordCapacityCheckpoint(ctx, pools)
}

func reconcilePoolCapacityCheckpoint(pool ReconcilePool) CapacityCheckpointPool {
	return CapacityCheckpointPool{
		ID:                             pool.ID,
		CapacityAcknowledged:           pool.CapacityAcknowledged,
		AcknowledgementPendingAge:      pool.AcknowledgementPendingAge,
		AcknowledgementPendingAgeValid: pool.AcknowledgementPendingAgeValid,
	}
}

func (r *recorder) recordCapacityCheckpoint(ctx context.Context, pools []CapacityCheckpointPool) {
	for _, pool := range pools {
		attrs := metric.WithAttributes(attribute.String("ci_runner.pool.id", pool.ID))
		r.capacityAcknowledged.Record(ctx, boolInt64(pool.CapacityAcknowledged), attrs)
		switch {
		case !pool.CapacityAcknowledged && pool.AcknowledgementPendingAgeValid:
			r.acknowledgementPendingAge.Record(ctx, max(0, pool.AcknowledgementPendingAge.Seconds()), attrs)
		case pool.CapacityAcknowledged:
			// Expire the synchronous-gauge series so cumulative OTLP exporters do
			// not keep reporting a stale pending age beside acknowledged=1.
			r.acknowledgementPendingAge.Record(ctx, 0, attrs)
		}
	}
}

func (r *recorder) recordSnapshot(ctx context.Context, snapshot ReconcileSnapshot) {
	counts := make(map[string]map[string]int64, len(snapshot.Pools))
	active := make(map[string]int64, len(snapshot.Pools))
	for _, pool := range snapshot.Pools {
		attrs := metric.WithAttributes(attribute.String("ci_runner.pool.id", pool.ID))
		r.advertised.Record(ctx, int64(pool.Advertised), attrs)
		r.recordCapacityCheckpoint(ctx, []CapacityCheckpointPool{reconcilePoolCapacityCheckpoint(pool)})
		r.assigned.Record(ctx, int64(pool.Assigned), attrs)
		r.desired.Record(ctx, int64(pool.Desired), attrs)
		r.memoryAffordable.Record(ctx, int64(pool.AffordableWorkers), attrs)
		counts[pool.ID] = make(map[string]int64, len(workerStates))
	}
	for _, worker := range snapshot.Workers {
		if counts[worker.PoolID] == nil {
			continue
		}
		counts[worker.PoolID][worker.State]++
		if worker.State == "busy" {
			active[worker.PoolID]++
		}
	}
	for _, pool := range snapshot.Pools {
		for _, state := range workerStates {
			r.workers.Record(ctx, counts[pool.ID][state], metric.WithAttributes(
				attribute.String("ci_runner.pool.id", pool.ID),
				attribute.String("ci_runner.worker.state", state),
			))
		}
		r.activeJobs.Record(ctx, active[pool.ID], metric.WithAttributes(attribute.String("ci_runner.pool.id", pool.ID)))
		inventory := int64(0)
		for _, state := range workerStates {
			inventory += counts[pool.ID][state]
		}
		attrs := metric.WithAttributes(attribute.String("ci_runner.pool.id", pool.ID))
		r.inventoryWorkers.Record(ctx, inventory, attrs)
		locallyVisible := active[pool.ID] + counts[pool.ID]["starting"]
		gap := max(0, int64(pool.Assigned)-locallyVisible)
		r.assignmentGap.Record(ctx, gap, attrs)
		r.transientLag.Record(ctx, boolInt64(gap > 0), attrs)
	}
	r.cpuPercent.Record(ctx, snapshot.CPUPercent)
	r.availableMemory.Record(ctx, clampUint64(snapshot.AvailableMemoryBytes))
	r.memoryHeadroom.Record(ctx, clampUint64(snapshot.MemoryHeadroomBytes))
	r.resourceGate.Record(ctx, boolInt64(snapshot.ResourceGateBlocked))
	r.powerGate.Record(ctx, boolInt64(snapshot.PowerGateBlocked))
	if snapshot.CheckpointAgeValid {
		r.checkpointAge.Record(ctx, max(0, snapshot.CheckpointAge.Seconds()))
	}
}

func snapshotHasTransientAccountingLag(snapshot ReconcileSnapshot) bool {
	visible := make(map[string]int, len(snapshot.Pools))
	for _, worker := range snapshot.Workers {
		if worker.State == "busy" || worker.State == "starting" {
			visible[worker.PoolID]++
		}
	}
	for _, pool := range snapshot.Pools {
		if pool.Assigned > visible[pool.ID] {
			return true
		}
	}
	return false
}

func (r *recorder) WorkerRegistered(ctx context.Context, poolID, tier string, duration time.Duration, outcome WorkerStartOutcome) {
	if poolID == "" {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("ci_runner.pool.id", poolID),
		attribute.String("ci_runner.worker.resource.tier", normalizeResourceTier(tier)),
		attribute.String("ci_runner.worker.registration.outcome", string(outcome)),
	}
	r.workerRegisters.Add(ctx, 1, metric.WithAttributes(attrs...))
	r.workerRegisterTime.Record(ctx, max(0, duration.Seconds()), metric.WithAttributes(attrs...))
	r.recordLifecycleEventTime(ctx, poolID, tier, "registered", string(outcome))
	trace.SpanFromContext(ctx).AddEvent("worker.registered", trace.WithAttributes(attrs...))
}

func (r *recorder) WorkerStarted(ctx context.Context, poolID, tier string, duration time.Duration, outcome WorkerStartOutcome) {
	if poolID == "" {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("ci_runner.pool.id", poolID),
		attribute.String("ci_runner.worker.resource.tier", normalizeResourceTier(tier)),
		attribute.String("ci_runner.worker.start.outcome", string(outcome)),
	}
	r.workerStarts.Add(ctx, 1, metric.WithAttributes(attrs...))
	r.workerStartTime.Record(ctx, max(0, duration.Seconds()), metric.WithAttributes(attrs...))
	r.recordLifecycleEventTime(ctx, poolID, tier, "started", string(outcome))
	trace.SpanFromContext(ctx).AddEvent("worker.started", trace.WithAttributes(attrs...))
	if outcome == WorkerStartCanceled {
		r.cancellations.Add(ctx, 1, metric.WithAttributes(
			attribute.String("ci_runner.cancellation.source", "worker_start"),
			attribute.String("ci_runner.cancellation.classification", "controller_context"),
		))
	}
}

func (r *recorder) WorkerFinalized(ctx context.Context, poolID string, value WorkerFinalization) {
	if poolID == "" {
		return
	}
	outcome := ClassifyWorkerFinalization(value)
	tier := normalizeResourceTier(value.ResourceTier)
	baseAttributes := []attribute.KeyValue{
		attribute.String("ci_runner.pool.id", poolID),
		attribute.String("ci_runner.worker.resource.tier", tier),
		attribute.String("ci_runner.worker.finalization.outcome", string(outcome)),
	}
	r.workerFinalizes.Add(ctx, 1, metric.WithAttributes(baseAttributes...))
	r.workerFinalizeTime.Record(ctx, max(0, value.Duration.Seconds()), metric.WithAttributes(baseAttributes...))
	r.recordLifecycleEventTime(ctx, poolID, tier, "finalized", string(outcome))
	if outcome == WorkerFinalizationCanceled {
		r.cancellations.Add(ctx, 1, metric.WithAttributes(
			attribute.String("ci_runner.cancellation.source", "worker_finalization"),
			attribute.String("ci_runner.cancellation.classification", "controller_context"),
		))
	}
	if value.RecordResourceEvidence {
		r.recordWorkerResourceEvidence(ctx, poolID, tier, outcome, value.ResourceEvidence)
	}
}

func (r *recorder) ObserveJobStarted(ctx context.Context, poolID string, visibilityLag time.Duration) {
	if poolID == "" {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("ci_runner.pool.id", poolID)}
	r.jobsStarted.Add(ctx, 1, metric.WithAttributes(attrs...))
	r.jobStartLag.Record(ctx, max(0, visibilityLag.Seconds()), metric.WithAttributes(attrs...))
	r.recordLifecycleEventTime(ctx, poolID, "unknown", "job_started", "observed")
	trace.SpanFromContext(ctx).AddEvent("job.started", trace.WithAttributes(attrs...))
}

func (r *recorder) recordLifecycleEventTime(ctx context.Context, poolID, tier, event, outcome string) {
	r.lifecycleEventTime.Record(ctx, float64(time.Now().UnixNano())/float64(time.Second), metric.WithAttributes(
		attribute.String("ci_runner.pool.id", poolID),
		attribute.String("ci_runner.worker.resource.tier", normalizeResourceTier(tier)),
		attribute.String("ci_runner.worker.lifecycle.event", event),
		attribute.String("ci_runner.worker.lifecycle.outcome", outcome),
	))
}

func (r *recorder) recordWorkerResourceEvidence(ctx context.Context, poolID, tier string, finalizationOutcome WorkerFinalizationOutcome, evidence *WorkerResourceEvidence) {
	status := "unavailable"
	if evidence != nil {
		status = normalizeResourceOutcome(evidence.Status)
	}
	evidenceAttributes := []attribute.KeyValue{
		attribute.String("ci_runner.pool.id", poolID),
		attribute.String("ci_runner.worker.resource.tier", tier),
		attribute.String("ci_runner.worker.resource.outcome", status),
	}
	r.resourceEvidence.Add(ctx, 1, metric.WithAttributes(evidenceAttributes...))
	if evidence == nil || status == "missing" || status == "unavailable" || status == "invalid" {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("ci_runner.pool.id", poolID),
		attribute.String("ci_runner.worker.resource.tier", tier),
		attribute.String("ci_runner.worker.finalization.outcome", string(finalizationOutcome)),
	)
	if !slices.Contains(evidence.Missing, "memory.peak") {
		r.memoryPeak.Record(ctx, clampUint64(evidence.MemoryPeakBytes), attrs)
	}
	if !slices.Contains(evidence.Missing, "memory.swap.peak") {
		r.memorySwapPeak.Record(ctx, clampUint64(evidence.MemorySwapPeakBytes), attrs)
	}
	if !slices.Contains(evidence.Missing, "memory.events.oom") {
		r.oomEvents.Add(ctx, clampUint64(evidence.OOMEvents), attrs)
	}
	if !slices.Contains(evidence.Missing, "memory.events.oom_kill") {
		r.oomKillEvents.Add(ctx, clampUint64(evidence.OOMKillEvents), attrs)
	}
	if !slices.Contains(evidence.Missing, "cpu.stat.nr_periods") {
		r.cpuPeriods.Record(ctx, clampUint64(evidence.CPUPeriods), attrs)
	}
	if !slices.Contains(evidence.Missing, "cpu.stat.nr_throttled") {
		r.cpuThrottled.Record(ctx, clampUint64(evidence.CPUThrottledPeriods), attrs)
	}
	if !slices.Contains(evidence.Missing, "cpu.stat.throttled_usec") {
		r.cpuThrottledTime.Record(ctx, float64(evidence.CPUThrottledMicroseconds)/1_000_000, attrs)
	}
	if !slices.Contains(evidence.Missing, "pids.peak") {
		r.pidsPeak.Record(ctx, clampUint64(evidence.PIDsPeak), attrs)
	}
	if !slices.Contains(evidence.Missing, "io.stat") {
		r.ioRead.Record(ctx, clampUint64(evidence.IOReadBytes), attrs)
		r.ioWrite.Record(ctx, clampUint64(evidence.IOWriteBytes), attrs)
	}
}

func normalizeResourceTier(value string) string {
	switch value {
	case "default", "target_override", "unknown":
		return value
	default:
		return "unknown"
	}
}

func normalizeResourceOutcome(value string) string {
	switch value {
	case "complete", "partial", "missing", "unavailable", "invalid":
		return value
	default:
		return "invalid"
	}
}

func clampUint64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func (r *recorder) ObserveJobCompleted(ctx context.Context, poolID, result string, assigned bool) {
	if poolID == "" {
		return
	}
	normalized := normalizeJobResult(result)
	r.jobsCompleted.Add(ctx, 1, metric.WithAttributes(
		attribute.String("ci_runner.pool.id", poolID),
		attribute.String("ci_runner.job.result", normalized),
	))
	if normalized != "canceled" {
		return
	}
	classification := "before_assignment"
	if assigned {
		classification = "assigned"
	}
	r.cancellations.Add(ctx, 1, metric.WithAttributes(
		attribute.String("ci_runner.cancellation.source", "github_job"),
		attribute.String("ci_runner.cancellation.classification", classification),
	))
}

func normalizeJobResult(result string) string {
	switch strings.ToLower(strings.TrimSpace(result)) {
	case "succeeded", "success":
		return "succeeded"
	case "failed", "failure":
		return "failed"
	case "canceled", "cancelled":
		return "canceled"
	case "skipped":
		return "skipped"
	default:
		return "other"
	}
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

// NewManualRecorder returns a Recorder backed by a ManualReader for tests.
func NewManualRecorder() (Recorder, *sdkmetric.ManualReader, error) {
	reader := sdkmetric.NewManualReader()
	meters := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	traces := tracenoop.NewTracerProvider()
	recorder, err := newRecorder(traces, meters)
	if err != nil {
		return nil, nil, err
	}
	return recorder, reader, nil
}
