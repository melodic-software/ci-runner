package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/scaleset"
)

// phaseRecordingStore records the phase of every observed checkpoint the controller writes.
type phaseRecordingStore struct {
	StateStore

	mu     sync.Mutex
	phases []model.Phase
	codes  [][]string
}

func (s *phaseRecordingStore) SaveObserved(ctx context.Context, observed model.ObservedState) error {
	s.mu.Lock()
	s.phases = append(s.phases, observed.Phase)
	codes := make([]string, 0, len(observed.Problems))
	for _, problem := range observed.Problems {
		codes = append(codes, problem.Code)
	}
	s.codes = append(s.codes, codes)
	s.mu.Unlock()
	return s.StateStore.SaveObserved(ctx, observed)
}

func (s *phaseRecordingStore) assertNeverDegraded(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, phase := range s.phases {
		if phase == model.PhaseDegraded {
			t.Fatalf("checkpoint %d persisted phase=degraded with problems %v (all phases %v)", index, s.codes[index], s.phases)
		}
	}
}

// desiredFlippingInventory writes a new desired mode during its Nth List call, then blocks until the
// safety watcher cancels the pass, the way an operator's request lands mid-pass.
type desiredFlippingInventory struct {
	*testRuntime
	store  StateStore
	mode   model.Mode
	flipAt int

	mu    sync.Mutex
	calls int
}

func (i *desiredFlippingInventory) List(ctx context.Context) ([]model.Worker, error) {
	i.mu.Lock()
	i.calls++
	flip := i.calls == i.flipAt
	i.mu.Unlock()
	if flip {
		if err := i.store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: i.mode, UpdatedAt: time.Now().UTC()}); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return i.testRuntime.List(ctx)
}

// Regression for #491: a desired-mode change that cancels a pass must not persist the
// cancellation's own probe failures as a degraded checkpoint, which `--wait` reads as terminal.
func TestDesiredChangeMidPassNeverPersistsDegradedCheckpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		from, to model.Mode
		flipAt   int
	}{
		// Disabled mode starts no worker, so List call 2 is the post-poll refresh.
		{name: "after the poll", from: model.ModeDisabled, to: model.ModeGaming, flipAt: 2},
		// Enabled mode lists three times through the warm start's admission; call 4 is the pre-poll
		// refresh after that start.
		{name: "before the poll", from: model.ModeEnabled, to: model.ModeDisabled, flipAt: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness := newHarness(t, test.from)
			recorder := &phaseRecordingStore{StateStore: harness.store}
			harness.controller.deps.State = recorder
			harness.controller.deps.Workers = &desiredFlippingInventory{testRuntime: harness.runtime, store: harness.store, mode: test.to, flipAt: test.flipAt}
			if err := harness.controller.setWatchIntervalForTest(5 * time.Millisecond); err != nil {
				t.Fatal(err)
			}

			result, err := harness.controller.Step(context.Background())
			if err != nil {
				t.Fatalf("step error = %v", err)
			}
			recorder.assertNeverDegraded(t)
			if result.Observed.Phase == model.PhaseDegraded || len(result.Observed.Pools) != 1 || result.Observed.Pools[0].MaxCapacity != 0 {
				t.Fatalf("rerun observed = phase %s pools %#v", result.Observed.Phase, result.Observed.Pools)
			}
		})
	}
}

// Regression for #511: once gaming is requested, an assignment no worker holds never restarts
// Docker Desktop or creates a worker, and the host still reaches gaming.
func TestGamingDrainNeverRestartsDesktopForAssignedJobs(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeGaming)
	logs := &testLogSink{}
	harness.controller.deps.Logs = logs
	// The operator's `wsl --shutdown` killed the busy workers; GitHub still counts their jobs.
	harness.desktop.status = model.DesktopStatus{}
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{TotalAssignedJobs: 2}
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseDraining,
		Pools: []model.PoolObservation{{
			ID: "org", ScaleSetID: 1, ListenerID: "listener-org", TotalAssignedJobs: 2, DrainServiceCapacity: 3,
			CapacityAcknowledged: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if harness.desktop.startCount() != 0 || harness.runtime.startCount() != 0 || logs.hasCode("desktop-started") {
		t.Fatalf("gaming started Docker Desktop %d times and %d workers", harness.desktop.startCount(), harness.runtime.startCount())
	}
	// A correctly gamed host carries no problem, so `host doctor` stays healthy; the log keeps the trail.
	if result.Observed.Phase != model.PhaseGaming || len(result.Observed.Problems) != 0 {
		t.Fatalf("phase = %s problems = %#v, want gaming with none", result.Observed.Phase, result.Observed.Problems)
	}
	if !logs.hasCode("gaming-assigned-job-deferred") {
		t.Fatalf("controller log lacks gaming-assigned-job-deferred: %s", logs)
	}
}

// The full #511 drain: the running job finishes, then the controller stops Docker Desktop and WSL,
// with no checkpoint along the way that `--wait` would read as a failure.
func TestGamingDrainWaitsForRunningJobThenTearsDownWithoutDegrading(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeGaming)
	recorder := &phaseRecordingStore{StateStore: harness.store}
	harness.controller.deps.State = recorder
	logs := &testLogSink{}
	harness.controller.deps.Logs = logs
	harness.desktop.status.RunningWSLCount = 2
	harness.runtime.workers = []model.Worker{{ID: "busy", Name: "busy", PoolID: "org", State: model.WorkerBusy, JobID: "job-1"}}
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{TotalAssignedJobs: 1}

	first, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed.Phase != model.PhaseDraining || harness.desktop.stopCount() != 0 || harness.desktop.wslCount() != 0 {
		t.Fatalf("busy drain = phase %s stops %d wsl %d", first.Observed.Phase, harness.desktop.stopCount(), harness.desktop.wslCount())
	}

	// The job finishes: its one-job worker exits and GitHub stops counting the assignment.
	harness.runtime.mu.Lock()
	harness.runtime.workers = nil
	harness.runtime.mu.Unlock()
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{}

	var last ReconcileResult
	for range 3 {
		if last, err = harness.controller.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if last.Observed.Phase != model.PhaseGaming || harness.desktop.stopCount() != 1 || harness.desktop.wslCount() != 1 {
		t.Fatalf("teardown = phase %s stops %d wsl %d", last.Observed.Phase, harness.desktop.stopCount(), harness.desktop.wslCount())
	}
	if harness.desktop.startCount() != 0 || harness.runtime.startCount() != 0 {
		t.Fatalf("gaming drain started Docker Desktop %d times and %d workers", harness.desktop.startCount(), harness.runtime.startCount())
	}
	recorder.assertNeverDegraded(t)
	for _, code := range []string{"desktop-stopped", "wsl-shutdown"} {
		if !logs.hasCode(code) {
			t.Fatalf("controller log lacks %s: %s", code, logs)
		}
	}
}

func TestEnabledDesktopStartIsLogged(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	logs := &testLogSink{}
	harness.controller.deps.Logs = logs
	harness.desktop.status = model.DesktopStatus{}

	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if harness.desktop.startCount() != 1 || !logs.hasCode("desktop-started") {
		t.Fatalf("starts = %d, log = %s", harness.desktop.startCount(), logs)
	}
}

func (s *testLogSink) hasCode(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event.Code == code {
			return true
		}
	}
	return false
}
