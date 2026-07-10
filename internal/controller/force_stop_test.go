package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/melodic-software/ci-runner/internal/model"
)

func TestForceStopRequiresExactFreshPreview(t *testing.T) {
	t.Parallel()
	runtime := &testRuntime{workers: []model.Worker{{ID: "worker-1", PoolID: "org", State: model.WorkerBusy, JobID: "job-1"}}}
	preview, err := ForceStopPreview(context.Background(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.workers[0].JobID = "job-2"
	runtime.mu.Unlock()
	actual, err := ExecuteForceStop(context.Background(), runtime, preview)
	if !errors.Is(err, ErrForceStopStateChanged) {
		t.Fatalf("error = %v", err)
	}
	if len(actual) != 1 || actual[0].JobID != "job-2" {
		t.Fatalf("actual = %#v", actual)
	}
	if len(runtime.forceStops()) != 0 {
		t.Fatal("stale preview terminated work")
	}
}

func TestForceStopExecutesConfirmedSnapshot(t *testing.T) {
	t.Parallel()
	runtime := &testRuntime{workers: []model.Worker{
		{ID: "worker-2", PoolID: "org", State: model.WorkerBusy, JobID: "job-2"},
		{ID: "worker-1", PoolID: "org", State: model.WorkerIdle},
	}}
	preview, _ := ForceStopPreview(context.Background(), runtime)
	stopped, err := ExecuteForceStop(context.Background(), runtime, preview)
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 2 || len(runtime.forceStops()) != 2 {
		t.Fatalf("stopped = %#v calls = %#v", stopped, runtime.forceStops())
	}
}
