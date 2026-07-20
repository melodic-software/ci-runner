package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/scaleset"
)

func TestResourceRecoveryInterruptsLongPollAtReconcileCadence(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		harness := newHarness(t, model.ModeEnabled)
		// Inside the bubble the cadence ticker advances logical time, so the
		// window is sized to a handful of ticks rather than the production
		// minute; only their ratio matters to what is exercised.
		harness.controller.config.Controller.ReconcileInterval.Duration = 100 * time.Millisecond
		harness.controller.config.Resources.CPUHysteresisWindow.Duration = 300 * time.Millisecond
		now := harness.now
		if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
			SchemaVersion: 1, Phase: model.PhaseResourceConstrained, HeartbeatAt: now.Add(-time.Minute),
			Pools:        []model.PoolObservation{{ID: "org", ScaleSetID: 1, ListenerID: "listener-org", MaxCapacity: 0, CapacityAcknowledged: true}},
			ResourceGate: model.ResourceGateState{Blocked: true, Reason: model.ResourceGateReasonCPU},
		}); err != nil {
			t.Fatal(err)
		}
		blocking := newFirstBlockingScaleSet(harness.scaleSets)
		harness.controller.deps.ScaleSets = blocking
		done := make(chan ReconcileResult, 1)
		go func() {
			result, _ := harness.controller.Step(context.Background())
			done <- result
		}()
		// Wait for the zero-capacity listener poll to durably block; by then the
		// recovery-start checkpoint has been written.
		synctest.Wait()

		checkpoint, err := harness.store.LoadObserved(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if checkpoint.ResourceGate.HealthySince == nil || !checkpoint.ResourceGate.HealthySince.Equal(now) {
			t.Fatalf("healthy recovery start was not checkpointed before the long poll: %#v", checkpoint.ResourceGate)
		}
		// Blocking on done lets the bubble auto-advance the cadence ticker past
		// the hysteresis window, firing recovery without a manual clock advance.
		result := <-done
		if result.Observed.Phase != model.PhaseReady || result.Observed.ResourceGate.Blocked {
			t.Fatalf("resource gate did not recover: %#v", result.Observed)
		}
		if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[0 3]" {
			t.Fatalf("advertised capacities = %v, want [0 3]", got)
		}
	})
}

func TestPowerRecoveryInterruptsLongPollAtReconcileCadence(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		harness := newHarness(t, model.ModeEnabled)
		// Window sized to a few cadence ticks of bubble time (see the resource
		// recovery test); only the window/interval ratio is under test here.
		harness.controller.config.Controller.ReconcileInterval.Duration = 100 * time.Millisecond
		harness.controller.config.Power.StableACWindow.Duration = 300 * time.Millisecond
		harness.controller.config.Power.Policy = config.PowerACOnly
		now := harness.now
		acSince := now
		if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
			SchemaVersion: 1, Phase: model.PhasePowerSuspended, HeartbeatAt: now.Add(-time.Minute),
			Pools:     []model.PoolObservation{{ID: "org", ScaleSetID: 1, ListenerID: "listener-org", MaxCapacity: 0, CapacityAcknowledged: true}},
			PowerGate: model.PowerGateState{ACSince: &acSince},
		}); err != nil {
			t.Fatal(err)
		}
		harness.controller.deps.Power = &mutablePower{snapshot: model.PowerSnapshot{ACConnected: true, ObservedAt: now}}
		blocking := newFirstBlockingScaleSet(harness.scaleSets)
		harness.controller.deps.ScaleSets = blocking
		done := make(chan ReconcileResult, 1)
		go func() {
			result, _ := harness.controller.Step(context.Background())
			done <- result
		}()
		synctest.Wait()
		// Blocking on done lets the cadence ticker advance the bubble clock past
		// the stable-AC window, firing recovery without a manual clock advance.
		result := <-done
		if result.Observed.Phase != model.PhaseReady {
			t.Fatalf("power gate did not recover: %#v", result.Observed)
		}
		if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[0 3]" {
			t.Fatalf("advertised capacities = %v, want [0 3]", got)
		}
	})
}

func TestInvalidResourceObservationCancelsNonzeroPollAndPreservesRacingAssignment(t *testing.T) {
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
	blocking := newAssignmentOnCancelScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	done := make(chan ReconcileResult, 1)
	go func() {
		result, _ := harness.controller.Step(context.Background())
		done <- result
	}()
	waitForSignal(t, blocking.entered, "nonzero listener poll did not begin")
	resources.set(model.ResourceSnapshot{})

	select {
	case result := <-done:
		if !result.Observed.ResourceGate.Blocked || result.Observed.ResourceGate.Reason != model.ResourceGateReasonInvalidObservation {
			t.Fatalf("invalid resource observation did not fail closed: %#v", result.Observed)
		}
		if harness.runtime.startCount() != 1 || len(result.Observed.Workers) != 1 {
			t.Fatalf("assignment that won the cancellation race was not serviced exactly once: starts=%d workers=%#v", harness.runtime.startCount(), result.Observed.Workers)
		}
	case <-time.After(time.Second):
		t.Fatal("invalid resource observation did not cancel nonzero listener capacity")
	}
	if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[3 0]" {
		t.Fatalf("advertised capacities = %v, want [3 0]", got)
	}
}

func TestPollCadenceObservationFailureIsDurablyLogged(t *testing.T) {
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
	logs := &testLogSink{}
	harness.controller.deps.Logs = logs
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	done := make(chan struct{}, 1)
	go func() {
		_, _ = harness.controller.Step(context.Background())
		done <- struct{}{}
	}()
	waitForSignal(t, blocking.entered, "listener poll did not begin")
	resources.setError(errors.New("resource monitor unavailable"))

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observation failure did not restart the listener poll")
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	found := false
	for _, event := range logs.events {
		if event.Code == "listener-cadence-observation-error" && event.Source == "resources" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("cadence observation diagnostics = %#v, want structured resources source", logs.events)
	}
}

func TestHeartbeatCheckpointDoesNotRestartStableLongPoll(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		harness := newHarness(t, model.ModeEnabled)
		harness.controller.config.Controller.ReconcileInterval.Duration = 100 * time.Millisecond
		now := harness.now
		if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
			SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: now.Add(-time.Minute),
			Pools: []model.PoolObservation{{ID: "org", ScaleSetID: 1, ListenerID: "listener-org", MaxCapacity: 3, CapacityAcknowledged: true}},
		}); err != nil {
			t.Fatal(err)
		}
		harness.runtime.workers = []model.Worker{{ID: "idle", Name: "idle", PoolID: "org", RunnerID: 1000, State: model.WorkerIdle}}
		notifying := &notifyingStateStore{StateStore: harness.store, saved: make(chan model.ObservedState, 8)}
		harness.controller.deps.State = notifying
		blocking := newFirstBlockingScaleSet(harness.scaleSets)
		harness.controller.deps.ScaleSets = blocking
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := harness.controller.Step(ctx)
			done <- err
		}()
		// Wait for the stable listener poll to durably block, then discard the
		// initial reconcile checkpoint so only cadence-driven saves remain.
		synctest.Wait()
		for len(notifying.saved) > 0 {
			<-notifying.saved
		}

		// Blocking here lets the cadence ticker fire; a heartbeat past the
		// initial checkpoint proves the loop ran without a manual clock advance.
		checkpoint := waitForObserved(t, notifying.saved, func(observed model.ObservedState) bool {
			return observed.HeartbeatAt.After(now)
		}, "heartbeat was not checkpointed on the reconcile cadence")
		if len(checkpoint.Pools) != 1 || !checkpoint.Pools[0].CapacityAcknowledged || checkpoint.Pools[0].MaxCapacity != 3 {
			t.Fatalf("stable acknowledged capacity was corrupted by heartbeat checkpoint: %#v", checkpoint.Pools)
		}
		if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[3]" {
			t.Fatalf("stable long poll was restarted: capacities=%v", got)
		}
		cancel()
		<-done
	})
}

func TestWorkerExitInterruptsLongPollAndStartsAssignedReplacements(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Controller.ReconcileInterval.Duration = 5 * time.Millisecond
	harness.runtime.workers = []model.Worker{{
		ID: "completed", Name: "runner-completed", PoolID: "org", RunnerID: 1000, State: model.WorkerIdle,
	}}
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{TotalAssignedJobs: 1}
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	done := make(chan ReconcileResult, 1)
	go func() {
		result, _ := harness.controller.Step(context.Background())
		done <- result
	}()
	waitForSignal(t, blocking.entered, "listener poll did not begin")

	harness.runtime.mu.Lock()
	harness.runtime.workers = nil
	harness.runtime.mu.Unlock()

	select {
	case result := <-done:
		if result.Observed.Pools[0].TotalAssignedJobs != 1 || result.Observed.Pools[0].DesiredWorkers != 2 {
			t.Fatalf("assigned replacement plan was not observed: %#v", result.Observed)
		}
		if harness.runtime.startCount() != 2 || len(result.Observed.Workers) != 2 {
			t.Fatalf("completed worker blocked replacement starts: starts=%d workers=%#v", harness.runtime.startCount(), result.Observed.Workers)
		}
	case <-time.After(time.Second):
		t.Fatal("completed worker remained stale behind the listener long poll")
	}
	if got := blocking.capacitiesSnapshot(); len(got) < 2 {
		t.Fatalf("worker exit did not restart listener poll: capacities=%v", got)
	}
}

func TestMemorySlotFlapDoesNotRestartNonzeroLongPoll(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Controller.ReconcileInterval.Duration = 5 * time.Millisecond
	now := harness.now
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseDegraded, HeartbeatAt: now,
		Pools: []model.PoolObservation{{
			ID: "org", ScaleSetID: 1, ListenerID: "listener-org",
			TotalAssignedJobs: 3, MaxCapacity: 2, CapacityAcknowledged: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	resources := &mutableResources{snapshot: model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 33 << 30, CPUUtilizationPercent: 10}}
	harness.controller.deps.Resources = resources
	notifying := &notifyingStateStore{StateStore: harness.store, saved: make(chan model.ObservedState, 8)}
	harness.controller.deps.State = notifying
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := harness.controller.Step(ctx)
		done <- err
	}()
	waitForSignal(t, blocking.entered, "memory-clamped listener poll did not begin")

	resources.set(model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 41 << 30, CPUUtilizationPercent: 10})
	waitForObserved(t, notifying.saved, func(observed model.ObservedState) bool {
		return len(observed.Pools) == 1 && observed.Pools[0].DesiredWorkers == 3
	}, "raised memory affordability was not checkpointed during the open poll")
	resources.set(model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 33 << 30, CPUUtilizationPercent: 10})
	waitForObserved(t, notifying.saved, func(observed model.ObservedState) bool {
		return len(observed.Pools) == 1 && observed.Pools[0].DesiredWorkers == 2
	}, "lowered memory affordability was not checkpointed during the open poll")

	if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[2]" {
		t.Fatalf("memory slot flap restarted the open listener poll: capacities=%v", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled listener poll did not stop")
	}
}

func TestPendingZeroToOneCapacityHoldsInsideMemoryBandBeforeAcknowledgement(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Controller.ReconcileInterval.Duration = 5 * time.Millisecond
	harness.controller.config.GitHub.Targets[0].WarmIdle = 0
	now := harness.now
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: now,
		Pools: []model.PoolObservation{{
			ID: "org", ScaleSetID: 1, ListenerID: "listener-org",
			MaxCapacity: 0, CapacityAcknowledged: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// With an 8 GiB worker and 25% increase margin, 26 GiB available is the
	// first-slot upper boundary above the 16 GiB reserve floor.
	resources := &mutableResources{snapshot: model.ResourceSnapshot{
		TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 26 << 30, CPUUtilizationPercent: 10,
	}}
	harness.controller.deps.Resources = resources
	notifying := &notifyingStateStore{StateStore: harness.store, saved: make(chan model.ObservedState, 8)}
	harness.controller.deps.State = notifying
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := harness.controller.Step(ctx)
		done <- err
	}()
	waitForSignal(t, blocking.entered, "zero-to-one listener poll did not begin")
	for len(notifying.saved) > 0 {
		<-notifying.saved
	}

	// Move below the growth threshold but remain above the raw one-slot
	// boundary. The in-flight one is the hysteresis state until this poll acks.
	resources.set(model.ResourceSnapshot{
		TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 25 << 30, CPUUtilizationPercent: 10,
	})
	checkpoint := waitForObserved(t, notifying.saved, func(observed model.ObservedState) bool {
		return observed.Resources.AvailableMemoryBytes == 25<<30
	}, "in-band memory observation was not checkpointed during the pending poll")
	if len(checkpoint.Pools) != 1 || checkpoint.Pools[0].CapacityAcknowledged || checkpoint.Pools[0].MaxCapacity != 0 {
		t.Fatalf("pending acknowledgement state was corrupted: %#v", checkpoint.Pools)
	}
	if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[1]" {
		t.Fatalf("in-band pending capacity restarted the listener poll: capacities=%v", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled pending listener poll did not stop")
	}
}

func TestMemoryWithdrawalRestartsNonzeroLongPollAndServicesRemainder(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Controller.ReconcileInterval.Duration = 5 * time.Millisecond
	now := harness.now
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: now,
		Pools: []model.PoolObservation{{
			ID: "org", ScaleSetID: 1, ListenerID: "listener-org",
			TotalAssignedJobs: 3, MaxCapacity: 3, CapacityAcknowledged: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{TotalAssignedJobs: 3}
	resources := &mutableResources{snapshot: model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 41 << 30, CPUUtilizationPercent: 10}}
	harness.controller.deps.Resources = resources
	logs := &testLogSink{}
	harness.controller.deps.Logs = logs
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	done := make(chan ReconcileResult, 1)
	go func() {
		result, _ := harness.controller.Step(context.Background())
		done <- result
	}()
	waitForSignal(t, blocking.entered, "memory-funded listener poll did not begin")

	resources.set(model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 33 << 30, CPUUtilizationPercent: 10})

	select {
	case result := <-done:
		if harness.runtime.startCount() != 2 {
			t.Fatalf("serviceable assigned jobs were not started after the withdrawal restart: starts=%d", harness.runtime.startCount())
		}
		if len(result.Observed.Pools) != 1 || result.Observed.Pools[0].TotalAssignedJobs != 3 {
			t.Fatalf("authoritative assignments were not observed after the restart: %#v", result.Observed.Pools)
		}
	case <-time.After(time.Second):
		t.Fatal("memory withdrawal did not restart the listener poll")
	}
	if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[3 2]" {
		t.Fatalf("advertised capacities = %v, want [3 2]", got)
	}
	if !logs.contains("listener-poll-superseded") {
		t.Fatalf("poll restart was not durably logged: %s", logs)
	}
}

func TestSupersededListenerPollIsNotRecordedAsStatisticsError(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Controller.ReconcileInterval.Duration = 5 * time.Millisecond
	now := harness.now
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: now,
		Pools: []model.PoolObservation{{
			ID: "org", ScaleSetID: 1, ListenerID: "listener-org",
			TotalAssignedJobs: 3, MaxCapacity: 3, CapacityAcknowledged: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	harness.scaleSets.Stats["statistics:1"] = scaleset.Statistics{TotalAssignedJobs: 3}
	resources := &mutableResources{snapshot: model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 41 << 30, CPUUtilizationPercent: 10}}
	harness.controller.deps.Resources = resources
	logs := &testLogSink{}
	harness.controller.deps.Logs = logs
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	type outcome struct {
		result ReconcileResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := harness.controller.Step(context.Background())
		done <- outcome{result: result, err: err}
	}()
	waitForSignal(t, blocking.entered, "memory-funded listener poll did not begin")

	// Withdraw memory below the advertised capacity. watchPollCadence cancels the
	// open long poll with errReconcileInputsChanged; the in-flight poll unblocks
	// with context.Canceled (firstBlockingScaleSet returns ctx.Err()) and Step
	// reruns. The canceled poll must not surface as a scale-set failure.
	resources.set(model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 33 << 30, CPUUtilizationPercent: 10})

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("benign supersession returned an error: %v", got.err)
		}
		if got.result.Observed.Phase == model.PhaseDegraded {
			t.Fatalf("benign supersession forced a degraded phase: %#v", got.result.Observed)
		}
	case <-time.After(time.Second):
		t.Fatal("memory withdrawal did not restart the listener poll")
	}
	if !logs.contains("listener-poll-superseded") {
		t.Fatalf("supersession precondition was not exercised: %s", logs)
	}
	if logs.contains("scale-set-statistics-error") {
		t.Fatalf("benign supersession was misreported as a scale-set poll failure: %s", logs)
	}
}

func TestPendingWithdrawalRerunPreservesRawAffordableRemainder(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.Controller.ReconcileInterval.Duration = 5 * time.Millisecond
	harness.controller.config.GitHub.Targets[0].WarmIdle = 0
	now := harness.now
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: now,
		Pools: []model.PoolObservation{{
			ID: "org", ScaleSetID: 1, ListenerID: "listener-org",
			MaxCapacity: 0, CapacityAcknowledged: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// With an 8 GiB worker, a 16 GiB reserve floor (25% of 64 GiB), and a 25%
	// increase margin: 34 GiB available affords two slots from scratch
	// (18 GiB headroom clears the two-slot growth margin), so the pending
	// poll starts by advertising 2.
	resources := &mutableResources{snapshot: model.ResourceSnapshot{
		TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 34 << 30, CPUUtilizationPercent: 10,
	}}
	harness.controller.deps.Resources = resources
	blocking := newFirstBlockingScaleSet(harness.scaleSets)
	harness.controller.deps.ScaleSets = blocking
	done := make(chan ReconcileResult, 1)
	go func() {
		result, _ := harness.controller.Step(context.Background())
		done <- result
	}()
	waitForSignal(t, blocking.entered, "pending zero-to-two listener poll did not begin")

	// Drop to 25 GiB available (9 GiB headroom): only one slot is raw
	// affordable, so the open poll is withdrawn from 2 down to that safe
	// remainder and Step reruns immediately. 9 GiB headroom is inside the
	// growth dead band for a *fresh* single slot (it needs 10 GiB to clear
	// the one-slot margin), so a rerun that forgets the in-flight baseline
	// would incorrectly re-derive this sample as new growth and collapse to
	// 0 instead of holding the one raw-affordable slot that was already
	// pending.
	resources.set(model.ResourceSnapshot{
		TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 25 << 30, CPUUtilizationPercent: 10,
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pending withdrawal did not restart the listener poll")
	}
	// The capacity actually advertised to GitHub on the rerun is what the
	// finding is about; [2 1] means the rerun held the raw-affordable
	// remainder, while [2 0] means it collapsed to zero.
	if got := blocking.capacitiesSnapshot(); fmt.Sprint(got) != "[2 1]" {
		t.Fatalf("advertised capacities = %v, want [2 1]", got)
	}
}

func TestCapacityRestoredFromZeroDetectsOnlyZeroToPositiveTransitions(t *testing.T) {
	t.Parallel()
	if capacityRestoredFromZero(map[string]int{"org": 2}, map[string]int{"org": 3}) {
		t.Fatal("nonzero growth was classified as a zero-capacity restoration")
	}
	if !capacityRestoredFromZero(map[string]int{"org": 0}, map[string]int{"org": 2}) {
		t.Fatal("zero-to-positive transition was not detected")
	}
	if !capacityRestoredFromZero(map[string]int{}, map[string]int{"org": 1}) {
		t.Fatal("missing prior pool capacity was not treated as zero")
	}
	if capacityRestoredFromZero(map[string]int{"org": 2}, map[string]int{"org": 0}) {
		t.Fatal("withdrawal was classified as a restoration")
	}
}

func TestSameWorkerInventoryIgnoresOrderingButDetectsLifecycleChanges(t *testing.T) {
	t.Parallel()
	left := []model.Worker{
		{ID: "a", PoolID: "org", State: model.WorkerIdle},
		{ID: "b", PoolID: "org", State: model.WorkerIdle},
	}
	right := []model.Worker{left[1], left[0]}
	if !sameWorkerInventory(left, right) {
		t.Fatal("worker ordering was treated as an inventory change")
	}
	right[0].State = model.WorkerExited
	if sameWorkerInventory(left, right) {
		t.Fatal("worker lifecycle change was ignored")
	}
}

func TestPoolAcknowledgementTransitionTimestampDoesNotResetWhilePending(t *testing.T) {
	t.Parallel()
	started := time.Unix(100, 0).UTC()
	now := started.Add(30 * time.Second)
	prior := model.PoolObservation{
		ScaleSetID: 1, ListenerID: "listener", MaxCapacity: 8,
		CapacityAcknowledged: false, UpdatedAt: started,
	}
	if got := poolAcknowledgementTransitionAt(prior, 1, "listener", 8, false, now); !got.Equal(started) {
		t.Fatalf("pending transition timestamp reset to %s", got)
	}
	reconciler := &Reconciler{pendingCapacity: map[string]int{"org": 8}}
	if got := reconciler.poolAcknowledgementTransitionAt("org", prior, 1, "listener", 8, 8, false, now); !got.Equal(started) {
		t.Fatalf("same pending capacity reset transition timestamp to %s", got)
	}
	if got := reconciler.poolAcknowledgementTransitionAt("org", prior, 1, "listener", 8, 6, false, now); !got.Equal(now) {
		t.Fatalf("changed pending capacity retained stale transition timestamp %s", got)
	}
	if got := poolAcknowledgementTransitionAt(prior, 1, "listener", 6, false, now); !got.Equal(now) {
		t.Fatalf("capacity transition timestamp = %s, want %s", got, now)
	}
	if got := poolAcknowledgementTransitionAt(prior, 1, "listener", 8, true, now); !got.Equal(now) {
		t.Fatalf("acknowledgement transition timestamp = %s, want %s", got, now)
	}
}

func TestFailedLongPollPreservesCadenceAcknowledgementTransitionAge(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		harness := newHarness(t, model.ModeEnabled)
		// A few cadence ticks fire over the age advance below, each re-checkpointing
		// the still-pending pool; the transition timestamp must survive both the
		// intervening cadence saves and the eventual poll failure.
		harness.controller.config.Controller.ReconcileInterval.Duration = 10 * time.Second
		started := harness.now
		if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
			SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: started,
			Pools: []model.PoolObservation{{
				ID: "org", ScaleSetID: 1, ListenerID: "listener-org", MaxCapacity: 8,
				CapacityAcknowledged: true, UpdatedAt: started.Add(-time.Minute),
			}},
		}); err != nil {
			t.Fatal(err)
		}
		notifying := &notifyingStateStore{StateStore: harness.store, saved: make(chan model.ObservedState, 8)}
		harness.controller.deps.State = notifying
		blocking := newBlockingPollFailure(harness.scaleSets)
		harness.controller.deps.ScaleSets = blocking
		type outcome struct {
			result ReconcileResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := harness.controller.Step(context.Background())
			done <- outcome{result: result, err: err}
		}()
		synctest.Wait()
		checkpoint := waitForObserved(t, notifying.saved, func(observed model.ObservedState) bool {
			return len(observed.Pools) == 1 && !observed.Pools[0].CapacityAcknowledged
		}, "pending acknowledgement transition was not checkpointed")
		transitionAt := checkpoint.Pools[0].UpdatedAt
		if !transitionAt.Equal(started) {
			t.Fatalf("transition started at %s, want %s", transitionAt, started)
		}
		// Age the clock past the transition so a bug that resets UpdatedAt to
		// now would diverge from started, then fail the poll.
		time.Sleep(30 * time.Second)
		close(blocking.release)
		got := <-done
		if got.err == nil {
			t.Fatal("failed listener poll returned no error")
		}
		if len(got.result.Observed.Pools) != 1 || !got.result.Observed.Pools[0].UpdatedAt.Equal(transitionAt) {
			t.Fatalf("failed poll reset transition age: %#v", got.result.Observed.Pools)
		}
	})
}

type mutableResources struct {
	mu       sync.Mutex
	snapshot model.ResourceSnapshot
	err      error
}

func (s *testLogSink) contains(code string) bool {
	_, found := s.find(code)
	return found
}

func (s *testLogSink) find(code string) (LogEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event.Code == code {
			return event, true
		}
	}
	return LogEvent{}, false
}

type blockingPollFailure struct {
	scaleset.Client
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingPollFailure(client scaleset.Client) *blockingPollFailure {
	return &blockingPollFailure{Client: client, entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingPollFailure) Statistics(context.Context, scaleset.Identity, int) (scaleset.Statistics, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return scaleset.Statistics{}, errors.New("listener poll failed")
}

func (m *mutableResources) Snapshot(context.Context) (model.ResourceSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot, m.err
}

func (m *mutableResources) set(snapshot model.ResourceSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot = snapshot
}

func (m *mutableResources) setError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

type assignmentOnCancelScaleSet struct {
	scaleset.Client
	entered chan struct{}
	mu      sync.Mutex
	calls   int
	maxima  []int
}

func newAssignmentOnCancelScaleSet(client scaleset.Client) *assignmentOnCancelScaleSet {
	return &assignmentOnCancelScaleSet{Client: client, entered: make(chan struct{})}
}

func (s *assignmentOnCancelScaleSet) Statistics(ctx context.Context, _ scaleset.Identity, maximum int) (scaleset.Statistics, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.maxima = append(s.maxima, maximum)
	if call == 1 {
		close(s.entered)
	}
	s.mu.Unlock()
	if call == 1 {
		<-ctx.Done()
	}
	return scaleset.Statistics{TotalAssignedJobs: 1}, nil
}

func (s *assignmentOnCancelScaleSet) capacitiesSnapshot() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.maxima...)
}

type notifyingStateStore struct {
	StateStore
	saved chan model.ObservedState
}

func (s *notifyingStateStore) SaveObserved(ctx context.Context, observed model.ObservedState) error {
	if err := s.StateStore.SaveObserved(ctx, observed); err != nil {
		return err
	}
	select {
	case s.saved <- observed:
	default:
	}
	return nil
}

func waitForObserved(t *testing.T, saved <-chan model.ObservedState, predicate func(model.ObservedState) bool, message string) model.ObservedState {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case observed := <-saved:
			if predicate(observed) {
				return observed
			}
		case <-timer.C:
			t.Fatal(message)
		}
	}
}
