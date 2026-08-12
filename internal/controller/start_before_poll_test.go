package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/model"
)

func TestWarmSlotStartsBeforeListenerPollReleased(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = harness.controller.Step(ctx)
		close(done)
	}()
	waitForSignal(t, blocking.entered, "listener poll did not begin")
	if got := harness.runtime.startCount(); got != 1 {
		t.Fatalf("warm slot start before poll release = %d, want 1", got)
	}
	cancel()
	<-done
	if got := harness.runtime.startCount(); got != 1 {
		t.Fatalf("duplicate warm start after poll = %d, want 1", got)
	}
}

func TestStartBeforePollDoesNotDuplicateWarmWorker(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := harness.runtime.startCount(); got != 1 {
		t.Fatalf("start count = %d, want exactly one warm worker", got)
	}
	if got := len(harness.runtime.snapshot()); got != 1 {
		t.Fatalf("worker count = %d, want 1", got)
	}
}

func TestPrePollStartDoesNotSelfCancelPollCadence(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		harness := newHarness(t, model.ModeEnabled)
		harness.controller.config.Controller.ReconcileInterval.Duration = 50 * time.Millisecond
		now := harness.now
		if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
			SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: now.Add(-time.Minute),
			Pools: []model.PoolObservation{{ID: "org", ScaleSetID: 1, ListenerID: "listener-org", MaxCapacity: 3, CapacityAcknowledged: true}},
		}); err != nil {
			t.Fatal(err)
		}
		blocking := newFirstBlockingScaleSet(harness.scaleSets)
		harness.controller.deps.ScaleSets = blocking
		ctx, cancel := context.WithCancel(context.Background())
		stepDone := make(chan struct{})
		go func() {
			_, _ = harness.controller.Step(ctx)
			close(stepDone)
		}()
		waitForSignal(t, blocking.entered, "listener poll did not begin")
		if got := harness.runtime.startCount(); got != 1 {
			t.Fatalf("pre-poll warm start = %d, want 1 before cadence ticks", got)
		}
		synctest.Wait()
		if got := blocking.capacitiesSnapshot(); len(got) != 1 {
			t.Fatalf("cadence self-canceled on pre-poll inventory: poll restarts = %v", got)
		}
		cancel()
		<-stepDone
	})
}

func TestCancelStormStillReplenishesWarmPool(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Controller.ReconcileInterval.Duration = 5 * time.Millisecond
	now := harness.now
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: now,
		Pools: []model.PoolObservation{{ID: "org", ScaleSetID: 1, ListenerID: "listener-org", MaxCapacity: 3, CapacityAcknowledged: true}},
	}); err != nil {
		t.Fatal(err)
	}
	resources := &mutableResources{snapshot: model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 64 << 30, CPUUtilizationPercent: 10}}
	harness.controller.deps.Resources = resources
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	done := make(chan ReconcileResult, 1)
	go func() {
		result, _ := harness.controller.Step(context.Background())
		done <- result
	}()
	waitForSignal(t, blocking.entered, "listener poll did not begin")
	if got := harness.runtime.startCount(); got != 1 {
		t.Fatalf("warm pool was not replenished before the cancel storm: starts=%d", got)
	}
	resources.set(model.ResourceSnapshot{})
	select {
	case result := <-done:
		if result.Observed.ResourceGate.Blocked && result.Observed.ResourceGate.Reason != model.ResourceGateReasonInvalidObservation {
			t.Fatalf("cancel storm did not fail closed on invalid observation: %#v", result.Observed.ResourceGate)
		}
	case <-time.After(time.Second):
		t.Fatal("invalid resource observation did not cancel the listener poll")
	}
	if got := harness.runtime.startCount(); got != 1 {
		t.Fatalf("cancel storm duplicated warm replenishment: starts=%d", got)
	}
}

func TestRecoveryOnlySkipsPrePollStarts(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	badStore := &corruptObservedStore{Store: harness.store}
	harness.controller.deps.State = badStore
	result, err := harness.controller.Step(context.Background())
	if err == nil || !badStore.quarantined {
		t.Fatalf("error=%v quarantined=%v", err, badStore.quarantined)
	}
	if got := harness.runtime.startCount(); got != 0 {
		t.Fatalf("recovery-only step started workers = %d, want 0", got)
	}
	if len(result.Plan.Start) != 0 {
		t.Fatalf("recovery-only plan still advertises starts: %#v", result.Plan.Start)
	}
}

func TestPrePollRefreshFailureAdvertisesZeroCapacity(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	refreshErr := errors.New("pre-poll Docker inventory failed")
	// Bootstrap inventory + two freshStartAllowed lists around the warm start,
	// then the dedicated pre-poll refresh.
	harness.runtime.listErr = refreshErr
	harness.runtime.listErrAt = 4
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := harness.controller.Step(ctx)
		done <- err
	}()
	waitForSignal(t, blocking.entered, "zero-capacity listener poll did not begin")
	if got := harness.runtime.startCount(); got != 1 {
		t.Fatalf("pre-poll warm start = %d, want 1 before the failed refresh", got)
	}
	if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[0]" {
		t.Fatalf("poll capacity after pre-poll refresh failure = %v, want [0]", got)
	}
	cancel()
	<-done
}

func TestBudgetFundedCapacitySurvivesHostHeadroomDipDuringCadence(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		harness := newHarness(t, model.ModeEnabled)
		harness.controller.config.Controller.ReconcileInterval.Duration = 50 * time.Millisecond
		harness.controller.config.GitHub.Targets[0].WarmIdle = 3
		harness.controller.config.Resources.WorkerMemoryBudget = config.ByteSize(36 << 30)
		harness.controller.engineMemoryTotal = 40 << 30
		resources := &mutableResources{snapshot: model.ResourceSnapshot{
			TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 20 << 30, CPUUtilizationPercent: 10,
		}}
		harness.controller.deps.Resources = resources
		blocking := newFirstBlockingScaleSet(harness.scaleSets)
		harness.controller.deps.ScaleSets = blocking
		ctx, cancel := context.WithCancel(context.Background())
		stepDone := make(chan struct{})
		go func() {
			_, _ = harness.controller.Step(ctx)
			close(stepDone)
		}()
		waitForSignal(t, blocking.entered, "budget-funded listener poll did not begin")
		if got := harness.runtime.startCount(); got != 3 {
			t.Fatalf("pre-poll warm burst = %d, want 3", got)
		}
		if got := blocking.capacitiesSnapshot(); len(got) != 1 || got[0] == 0 {
			t.Fatalf("initial advertised capacity = %v, want a positive single poll", got)
		}
		synctest.Wait()
		if got := blocking.capacitiesSnapshot(); len(got) != 1 {
			t.Fatalf("budget-funded cadence withdrew on host headroom: capacities=%v", got)
		}
		cancel()
		<-stepDone
	})
}
