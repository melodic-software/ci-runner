package controller

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

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
