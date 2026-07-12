package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/model"
)

func exactShutdownRequest(reason string) *control.ShutdownRequest {
	return &control.ShutdownRequest{
		Reason: reason, ExpectedProcessID: 1234, ExpectedVersion: "test-version",
	}
}

func TestControlHandlerCommitsOnlyExactAcceptedRequest(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	handler, err := NewControlHandler(harness.controller, 1234)
	if err != nil {
		t.Fatal(err)
	}
	request := control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "restart-1", Operation: control.OperationShutdown,
		Shutdown: exactShutdownRequest("release promotion"),
	}
	request.Shutdown.RestartViaTaskScheduler = true
	response := handler.Handle(context.Background(), request)
	if !response.OK || response.Status == nil || !response.Status.ShuttingDown {
		t.Fatalf("response = %#v", response)
	}
	if response.Status.RestartRequestID != request.RequestID {
		t.Fatalf("restart request ID = %q, want %q", response.Status.RestartRequestID, request.RequestID)
	}
	if harness.controller.ShuttingDown() {
		t.Fatal("shutdown began before response commit")
	}
	handler.CommitShutdown("different-request")
	if harness.controller.ShuttingDown() {
		t.Fatal("mismatched request committed shutdown")
	}
	handler.CommitShutdown(request.RequestID)
	if !harness.controller.ShuttingDown() {
		t.Fatal("accepted request did not begin shutdown")
	}
	status := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "status-after-commit", Operation: control.OperationStatus,
	})
	if !status.OK || status.Status == nil || status.Status.RestartRequestID != request.RequestID {
		t.Fatalf("committed restart status = %#v", status)
	}
	select {
	case signal := <-handler.ShutdownRequests():
		if signal.RequestID != request.RequestID || signal.Reason != "release promotion" || !signal.Restart {
			t.Fatalf("signal = %#v", signal)
		}
	default:
		t.Fatal("committed shutdown did not signal controller loop")
	}
}

func TestControlHandlerAbortReleasesOnlyUncommittedReservation(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	handler, err := NewControlHandler(harness.controller, 1234)
	if err != nil {
		t.Fatal(err)
	}
	first := control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "restart-1", Operation: control.OperationShutdown,
		Shutdown: exactShutdownRequest("failed response"),
	}
	if response := handler.Handle(context.Background(), first); !response.OK {
		t.Fatalf("first response = %#v", response)
	}
	handler.AbortShutdown("different-request")
	if response := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "restart-2", Operation: control.OperationShutdown,
		Shutdown: exactShutdownRequest("must still be reserved"),
	}); response.OK || response.ErrorCode != "shutdown-in-progress" {
		t.Fatalf("mismatched abort released reservation: %#v", response)
	}
	handler.AbortShutdown(first.RequestID)
	if harness.controller.ShuttingDown() {
		t.Fatal("aborted reservation stopped ordinary reconciliation")
	}
	second := control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "restart-2", Operation: control.OperationShutdown,
		Shutdown: exactShutdownRequest("retry"),
	}
	if response := handler.Handle(context.Background(), second); !response.OK {
		t.Fatalf("retry response = %#v", response)
	}
}

func TestControlHandlerAcceptsExactBusyDrainAndRejectsChangedCounts(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseReady,
		Pools:   []model.PoolObservation{{ID: "org", TotalAssignedJobs: 1}},
		Workers: []model.Worker{{ID: "worker", PoolID: "org", State: model.WorkerBusy, JobID: "job-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	handler, _ := NewControlHandler(harness.controller, 1234)
	shutdown := exactShutdownRequest("release promotion")
	shutdown.ExpectedAssignedJobCount = 1
	shutdown.ExpectedActiveJobCount = 1
	shutdown.ExpectedActiveWorkerCount = 1
	response := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "restart-1", Operation: control.OperationShutdown,
		Shutdown: shutdown,
	})
	if !response.OK || response.Status == nil || response.Status.ActiveJobCount != 1 {
		t.Fatalf("response = %#v", response)
	}
	changed := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "restart-2", Operation: control.OperationShutdown,
		Shutdown: exactShutdownRequest("stale preflight"),
	})
	if changed.OK || changed.ErrorCode != "shutdown-state-changed" {
		t.Fatalf("changed response = %#v", changed)
	}
}

func TestControlHandlerRejectsMatchingCountsForDifferentControllerIdentity(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*control.ShutdownRequest){
		"process ID": func(request *control.ShutdownRequest) { request.ExpectedProcessID++ },
		"version":    func(request *control.ShutdownRequest) { request.ExpectedVersion = "different-version" },
	}
	for name, changeIdentity := range tests {
		name, changeIdentity := name, changeIdentity
		t.Run(name, func(t *testing.T) {
			harness := newHarness(t, model.ModeEnabled)
			handler, err := NewControlHandler(harness.controller, 1234)
			if err != nil {
				t.Fatal(err)
			}
			shutdown := exactShutdownRequest("stale controller identity")
			changeIdentity(shutdown)
			request := control.Request{
				SchemaVersion: control.SchemaVersion, RequestID: "identity-changed", Operation: control.OperationShutdown,
				Shutdown: shutdown,
			}
			response := handler.Handle(context.Background(), request)
			if response.OK || response.ErrorCode != "shutdown-state-changed" || !strings.Contains(response.Error, "identity changed") {
				t.Fatalf("response = %#v", response)
			}
			handler.CommitShutdown(request.RequestID)
			handler.mu.Lock()
			pendingRequestID, committed := handler.pendingRequestID, handler.committed
			handler.mu.Unlock()
			if pendingRequestID != "" || committed {
				t.Fatalf("identity mismatch reserved or committed shutdown: pending=%q committed=%t", pendingRequestID, committed)
			}
			if harness.controller.ShuttingDown() {
				t.Fatal("identity mismatch created a committable shutdown reservation")
			}
			select {
			case signal := <-handler.ShutdownRequests():
				t.Fatalf("identity mismatch emitted shutdown signal %#v", signal)
			default:
			}
		})
	}
}

func TestControlHandlerForceStopUsesSoleRuntimeAndExactPreview(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	workers := []model.Worker{{ID: "worker-1", PoolID: "org", Name: "runner-1", State: model.WorkerBusy, JobID: "job-1"}}
	harness.runtime.workers = append([]model.Worker(nil), workers...)
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseDraining,
		Pools: []model.PoolObservation{{ID: "org", MaxCapacity: 0, CapacityAcknowledged: true}}, Workers: workers,
	}); err != nil {
		t.Fatal(err)
	}
	handler, _ := NewControlHandler(harness.controller, 1234)
	preview := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "force-preview", Operation: control.OperationForceStopPreview,
	})
	if !preview.OK || len(preview.ForceStopTargets) != 1 || preview.ForceStopTargets[0].JobID != "job-1" {
		t.Fatalf("preview = %#v", preview)
	}
	executed := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "force-execute", Operation: control.OperationForceStopExecute,
		ForceStop: &control.ForceStopRequest{Expected: preview.ForceStopTargets},
	})
	if !executed.OK || len(harness.runtime.forceStops()) != 1 {
		t.Fatalf("execute = %#v forced=%#v", executed, harness.runtime.forceStops())
	}
}

func TestControlHandlerForceStopRejectsStalePreviewWithoutStopping(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	workers := []model.Worker{{ID: "worker-1", PoolID: "org", Name: "runner-1", State: model.WorkerBusy, JobID: "job-1"}}
	harness.runtime.workers = append([]model.Worker(nil), workers...)
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseDraining,
		Pools: []model.PoolObservation{{ID: "org", MaxCapacity: 0, CapacityAcknowledged: true}}, Workers: workers,
	}); err != nil {
		t.Fatal(err)
	}
	handler, _ := NewControlHandler(harness.controller, 1234)
	preview := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "force-preview", Operation: control.OperationForceStopPreview,
	})
	harness.runtime.mu.Lock()
	harness.runtime.workers[0].JobID = "job-2"
	harness.runtime.mu.Unlock()
	response := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "force-execute", Operation: control.OperationForceStopExecute,
		ForceStop: &control.ForceStopRequest{Expected: preview.ForceStopTargets},
	})
	if response.OK || response.ErrorCode != "force-stop-state-changed" || len(harness.runtime.forceStops()) != 0 {
		t.Fatalf("response = %#v forced=%#v", response, harness.runtime.forceStops())
	}
}

func TestControlHandlerForceStopRequiresObservedZeroCapacity(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseDraining,
		Pools: []model.PoolObservation{{ID: "org", MaxCapacity: 1, CapacityAcknowledged: true}},
	}); err != nil {
		t.Fatal(err)
	}
	handler, _ := NewControlHandler(harness.controller, 1234)
	response := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "force-preview", Operation: control.OperationForceStopPreview,
	})
	if response.OK || response.ErrorCode != "force-stop-not-drained" {
		t.Fatalf("response = %#v", response)
	}
}

func TestControlHandlerForceStopRejectsUnacknowledgedNumericZero(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeDisabled)
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseDraining,
		Pools: []model.PoolObservation{{ID: "org", MaxCapacity: 0, CapacityAcknowledged: false}},
	}); err != nil {
		t.Fatal(err)
	}
	handler, _ := NewControlHandler(harness.controller, 1234)
	response := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "force-preview", Operation: control.OperationForceStopPreview,
	})
	if response.OK || response.ErrorCode != "force-stop-not-drained" {
		t.Fatalf("response = %#v", response)
	}
}
