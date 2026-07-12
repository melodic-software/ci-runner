package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRecorderExportsAggregateFleetStateWithoutHighCardinalityIdentity(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	meters := metric.NewMeterProvider(metric.WithReader(reader))
	spans := tracetest.NewInMemoryExporter()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	defer func() {
		_ = meters.Shutdown(context.Background())
		_ = traces.Shutdown(context.Background())
	}()

	recorder, err := newRecorder(traces, meters)
	if err != nil {
		t.Fatal(err)
	}
	ctx, finish := recorder.BeginReconcile(context.Background())
	snapshot := ReconcileSnapshot{
		Valid: true, Phase: "ready", CPUPercent: 22.5, AvailableMemoryBytes: 24 << 30, CheckpointAge: 11 * time.Second,
		Pools: []ReconcilePool{{ID: "org", Advertised: 6, Assigned: 3, Desired: 3}},
		Workers: []ReconcileWorker{
			{PoolID: "org", State: "busy"},
			{PoolID: "org", State: "idle"},
			{PoolID: "org", State: "starting"},
		},
	}
	recorder.WorkerRegistered(ctx, "org", "target_override", 25*time.Millisecond, WorkerStartSucceeded)
	recorder.WorkerStarted(ctx, "org", "target_override", 50*time.Millisecond, WorkerStartSucceeded)
	recorder.ObserveJobStarted(ctx, "org", 125*time.Millisecond)
	recorder.WorkerFinalized(ctx, "org", WorkerFinalization{
		ExitObserved: true, ExitCode: 0, ResourceTier: "target_override",
		ResourceEvidence: &WorkerResourceEvidence{
			Status: "complete", MemoryPeakBytes: 1986422374, OOMEvents: 0, OOMKillEvents: 0,
			CPUPeriods: 123, CPUThrottledPeriods: 7, CPUThrottledMicroseconds: 50000,
			PIDsPeak: 88, IOReadBytes: 2000000000, IOWriteBytes: 5500000000,
		},
	})
	recorder.ObserveJobCompleted(ctx, "org", "Succeeded", true)
	finish(snapshot, nil)

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	metrics := metricMap(collected)
	for _, name := range []string{
		"ci_runner.controller.reconcile.duration",
		"ci_runner.controller.observed.checkpoint.age",
		"ci_runner.capacity.advertised",
		"ci_runner.capacity.assigned",
		"ci_runner.capacity.desired",
		"ci_runner.workers",
		"ci_runner.jobs.active",
		"ci_runner.docker.inventory.workers",
		"ci_runner.accounting.assignment.gap",
		"ci_runner.accounting.transient_lag",
		"ci_runner.host.cpu.utilization",
		"ci_runner.host.memory.available",
		"ci_runner.gate.resource.blocked",
		"ci_runner.gate.power.blocked",
		"ci_runner.worker.starts",
		"ci_runner.worker.start.duration",
		"ci_runner.worker.registrations",
		"ci_runner.worker.registration.duration",
		"ci_runner.worker.finalizations",
		"ci_runner.worker.finalization.duration",
		"ci_runner.jobs.started",
		"ci_runner.jobs.start.visibility_lag",
		"ci_runner.worker.lifecycle.event.time",
		"ci_runner.jobs.completed",
		"ci_runner.worker.resource.evidence",
		"ci_runner.worker.memory.peak",
		"ci_runner.worker.memory.swap.peak",
		"ci_runner.worker.memory.oom.events",
		"ci_runner.worker.memory.oom_kill.events",
		"ci_runner.worker.cpu.periods",
		"ci_runner.worker.cpu.throttled.periods",
		"ci_runner.worker.cpu.throttled.duration",
		"ci_runner.worker.pids.peak",
		"ci_runner.worker.io.read",
		"ci_runner.worker.io.write",
	} {
		if _, found := metrics[name]; !found {
			t.Errorf("metric %q was not collected", name)
		}
	}
	if got := intGaugeValue(t, metrics["ci_runner.capacity.advertised"], "ci_runner.pool.id", "org"); got != 6 {
		t.Errorf("advertised capacity = %d, want 6", got)
	}
	if got := intGaugeValue(t, metrics["ci_runner.jobs.active"], "ci_runner.pool.id", "org"); got != 1 {
		t.Errorf("active jobs = %d, want 1", got)
	}
	if got := intGaugeValue(t, metrics["ci_runner.accounting.assignment.gap"], "ci_runner.pool.id", "org"); got != 1 {
		t.Errorf("assignment gap = %d, want 1", got)
	}
	if got := intGaugeValue(t, metrics["ci_runner.accounting.transient_lag"], "ci_runner.pool.id", "org"); got != 1 {
		t.Errorf("transient accounting lag = %d, want 1", got)
	}
	if got := intGaugeValueWithTwoAttributes(t, metrics["ci_runner.workers"], "ci_runner.pool.id", "org", "ci_runner.worker.state", "exited"); got != 0 {
		t.Errorf("exited workers = %d, want explicit zero", got)
	}
	if got := intHistogramSum(t, metrics["ci_runner.worker.memory.peak"]); got != 1986422374 {
		t.Errorf("memory peak histogram sum = %d, want 1986422374", got)
	}
	if got := intHistogramSum(t, metrics["ci_runner.worker.io.write"]); got != 5500000000 {
		t.Errorf("I/O write histogram sum = %d, want 5500000000", got)
	}
	assertLowCardinality(t, collected)

	gotSpans := spans.GetSpans()
	if len(gotSpans) != 1 || gotSpans[0].Name != "controller.reconcile" {
		t.Fatalf("spans = %#v", gotSpans)
	}
	if gotSpans[0].Status.Code != codes.Unset || attributeValue(gotSpans[0].Attributes, "ci_runner.phase") != "ready" || attributeValue(gotSpans[0].Attributes, "ci_runner.accounting.classification") != "transientAccountingLag" {
		t.Fatalf("reconcile span = %#v", gotSpans[0])
	}
	if len(gotSpans[0].Events) != 3 {
		t.Fatalf("reconcile lifecycle events = %#v", gotSpans[0].Events)
	}
}

func TestRecorderClassifiesExpectedCancellationWithoutReconcileError(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	meters := metric.NewMeterProvider(metric.WithReader(reader))
	spans := tracetest.NewInMemoryExporter()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	defer func() {
		_ = meters.Shutdown(context.Background())
		_ = traces.Shutdown(context.Background())
	}()
	recorder, err := newRecorder(traces, meters)
	if err != nil {
		t.Fatal(err)
	}
	_, finish := recorder.BeginReconcile(context.Background())
	finish(ReconcileSnapshot{}, context.Canceled)

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	metrics := metricMap(collected)
	if _, found := metrics["ci_runner.controller.reconcile.errors"]; found {
		t.Fatal("expected controller cancellation was counted as a reconciliation error")
	}
	if got := intSumValue(t, metrics["ci_runner.cancellations"], "ci_runner.cancellation.classification", "shutdown"); got != 1 {
		t.Fatalf("shutdown cancellations = %d, want 1", got)
	}
	if got := spans.GetSpans(); len(got) != 1 || got[0].Status.Code == codes.Error || attributeValue(got[0].Attributes, "ci_runner.reconcile.result") != "canceled" {
		t.Fatalf("canceled reconcile span = %#v", got)
	}
}

func TestRecorderMarksUnexpectedReconcileFailure(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	meters := metric.NewMeterProvider(metric.WithReader(reader))
	spans := tracetest.NewInMemoryExporter()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	defer func() {
		_ = meters.Shutdown(context.Background())
		_ = traces.Shutdown(context.Background())
	}()
	recorder, err := newRecorder(traces, meters)
	if err != nil {
		t.Fatal(err)
	}
	_, finish := recorder.BeginReconcile(context.Background())
	finish(ReconcileSnapshot{}, errors.New("poll failed"))

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	if got := intSumValue(t, metricMap(collected)["ci_runner.controller.reconcile.errors"], "", ""); got != 1 {
		t.Fatalf("reconcile errors = %d, want 1", got)
	}
	if got := spans.GetSpans(); len(got) != 1 || got[0].Status.Code != codes.Error {
		t.Fatalf("failed reconcile span = %#v", got)
	}
}

func TestOneShotBrokerTaskCanceledLogDoesNotOverrideSuccessfulContainerExit(t *testing.T) {
	t.Parallel()
	// TaskCanceledException is diagnostic text from the stock runner stopping
	// its broker long-poll. It is intentionally absent from this classifier.
	if got := ClassifyWorkerFinalization(WorkerFinalization{ExitObserved: true, ExitCode: 0}); got != WorkerFinalizationCompleted {
		t.Fatalf("zero-exit finalization = %q, want completed", got)
	}
	if got := ClassifyWorkerFinalization(WorkerFinalization{ExitObserved: true, ExitCode: 1}); got != WorkerFinalizationWorkerError {
		t.Fatalf("nonzero-exit finalization = %q, want worker error", got)
	}
	if got := ClassifyWorkerFinalization(WorkerFinalization{Err: context.Canceled}); got != WorkerFinalizationCanceled {
		t.Fatalf("context-canceled finalization = %q, want canceled", got)
	}
}

func metricMap(resourceMetrics metricdata.ResourceMetrics) map[string]metricdata.Metrics {
	result := map[string]metricdata.Metrics{}
	for _, scope := range resourceMetrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			result[current.Name] = current
		}
	}
	return result
}

func intGaugeValue(t *testing.T, current metricdata.Metrics, key, value string) int64 {
	t.Helper()
	gauge, ok := current.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("metric %q data = %T, want int64 gauge", current.Name, current.Data)
	}
	for _, point := range gauge.DataPoints {
		if key == "" || setValue(point.Attributes, key) == value {
			return point.Value
		}
	}
	t.Fatalf("metric %q has no point %s=%s", current.Name, key, value)
	return 0
}

func intGaugeValueWithTwoAttributes(t *testing.T, current metricdata.Metrics, key1, value1, key2, value2 string) int64 {
	t.Helper()
	gauge, ok := current.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("metric %q data = %T, want int64 gauge", current.Name, current.Data)
	}
	for _, point := range gauge.DataPoints {
		if setValue(point.Attributes, key1) == value1 && setValue(point.Attributes, key2) == value2 {
			return point.Value
		}
	}
	t.Fatalf("metric %q has no point %s=%s,%s=%s", current.Name, key1, value1, key2, value2)
	return 0
}

func intSumValue(t *testing.T, current metricdata.Metrics, key, value string) int64 {
	t.Helper()
	sum, ok := current.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q data = %T, want int64 sum", current.Name, current.Data)
	}
	for _, point := range sum.DataPoints {
		if key == "" || setValue(point.Attributes, key) == value {
			return point.Value
		}
	}
	t.Fatalf("metric %q has no point %s=%s", current.Name, key, value)
	return 0
}

func intHistogramSum(t *testing.T, current metricdata.Metrics) int64 {
	t.Helper()
	histogram, ok := current.Data.(metricdata.Histogram[int64])
	if !ok || len(histogram.DataPoints) != 1 {
		t.Fatalf("metric %q data = %T with %d points, want one int64 histogram point", current.Name, current.Data, len(histogram.DataPoints))
	}
	return histogram.DataPoints[0].Sum
}

func setValue(set attribute.Set, key string) string {
	value, found := set.Value(attribute.Key(key))
	if !found {
		return ""
	}
	return value.AsString()
}

func attributeValue(attributes []attribute.KeyValue, key string) string {
	for _, current := range attributes {
		if string(current.Key) == key {
			return current.Value.AsString()
		}
	}
	return ""
}

func assertLowCardinality(t *testing.T, resourceMetrics metricdata.ResourceMetrics) {
	t.Helper()
	allowed := map[string]struct{}{
		"ci_runner.pool.id": {}, "ci_runner.worker.state": {}, "ci_runner.reconcile.result": {},
		"ci_runner.worker.start.outcome": {}, "ci_runner.worker.finalization.outcome": {},
		"ci_runner.worker.registration.outcome": {},
		"ci_runner.job.result":                  {}, "ci_runner.cancellation.source": {},
		"ci_runner.cancellation.classification": {},
		"ci_runner.worker.resource.tier":        {}, "ci_runner.worker.resource.outcome": {},
		"ci_runner.worker.lifecycle.event": {}, "ci_runner.worker.lifecycle.outcome": {},
	}
	for _, scope := range resourceMetrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			for _, set := range aggregationAttributes(current.Data) {
				for _, item := range set.ToSlice() {
					if _, ok := allowed[string(item.Key)]; !ok {
						t.Errorf("metric %q contains unreviewed attribute key %q", current.Name, item.Key)
					}
					if strings.Contains(item.Value.Emit(), "runner-") || strings.Contains(item.Value.Emit(), "job-") {
						t.Errorf("metric %q contains high-cardinality identity %q", current.Name, item.Value.Emit())
					}
				}
			}
		}
	}
}

func aggregationAttributes(data metricdata.Aggregation) []attribute.Set {
	var result []attribute.Set
	switch value := data.(type) {
	case metricdata.Gauge[int64]:
		for _, point := range value.DataPoints {
			result = append(result, point.Attributes)
		}
	case metricdata.Gauge[float64]:
		for _, point := range value.DataPoints {
			result = append(result, point.Attributes)
		}
	case metricdata.Sum[int64]:
		for _, point := range value.DataPoints {
			result = append(result, point.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, point := range value.DataPoints {
			result = append(result, point.Attributes)
		}
	case metricdata.Histogram[int64]:
		for _, point := range value.DataPoints {
			result = append(result, point.Attributes)
		}
	}
	return result
}

func TestNormalizeJobResultIsBounded(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"Succeeded": "succeeded", "FAILED": "failed", "cancelled": "canceled",
		"Skipped": "skipped", "secret-job-result-123": "other",
	}
	for input, want := range tests {
		if got := normalizeJobResult(input); got != want {
			t.Errorf("normalizeJobResult(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMetricExportTimingUsesPositiveMilliseconds(t *testing.T) {
	t.Parallel()
	lookup := mapLookup(map[string]string{"OTEL_METRIC_EXPORT_INTERVAL": "1500"})
	got, err := millisecondsEnvironment(lookup, "OTEL_METRIC_EXPORT_INTERVAL", time.Minute)
	if err != nil || got != 1500*time.Millisecond {
		t.Fatalf("duration = %s, error = %v", got, err)
	}
}
