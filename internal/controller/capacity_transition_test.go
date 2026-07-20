package controller

import (
	"context"
	"reflect"
	"testing"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/model"
)

func TestReconcilerAcknowledgesDecreaseBeforeCrossPoolIncrease(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	harness.controller.config.GitHub.Targets[0].Priority = 10
	harness.controller.config.GitHub.Targets = append(harness.controller.config.GitHub.Targets, config.Target{
		ID: "build", URL: "https://github.com/melodic-software", Scope: config.ScopeOrganization,
		ClientID: "Iv23liABCDEF1234", InstallationID: 12345, SecretID: "melodic-org-host", RunnerGroup: "ci-local-melo-desk-001",
		ScaleSetName: "melodic-build-ubuntu-24.04-x64", Labels: []string{"melodic-build-ubuntu-24.04-x64"},
		WarmIdle: 0, MaxCapacity: 1, Priority: 0,
	})
	now := harness.now
	if err := harness.store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: now,
		Pools: []model.PoolObservation{
			{ID: "org", ScaleSetID: 1, ListenerID: "listener-org", MaxCapacity: 3, CapacityAcknowledged: true},
			{ID: "build", ScaleSetID: 2, ListenerID: "listener-build", MaxCapacity: 0, CapacityAcknowledged: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	first, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := observedCapacities(first.Observed); !reflect.DeepEqual(got, map[string]int{"org": 2, "build": 0}) {
		t.Fatalf("first acknowledged capacities = %#v, want decrease-only handoff", got)
	}
	firstCalls := len(harness.scaleSets.SnapshotCalls())

	second, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := observedCapacities(second.Observed); !reflect.DeepEqual(got, map[string]int{"org": 2, "build": 1}) {
		t.Fatalf("second acknowledged capacities = %#v, want completed handoff", got)
	}
	for _, call := range harness.scaleSets.SnapshotCalls()[firstCalls:] {
		if call.Operation == "statistics" && call.ScaleSetID == 2 && call.MaxCapacity != 1 {
			t.Fatalf("build increase after org decrease = %d, want 1", call.MaxCapacity)
		}
	}
}

func observedCapacities(observed model.ObservedState) map[string]int {
	result := make(map[string]int, len(observed.Pools))
	for _, pool := range observed.Pools {
		result[pool.ID] = pool.MaxCapacity
	}
	return result
}

func TestCapacityTransferDecreasesBeforeCrossPoolIncrease(t *testing.T) {
	t.Parallel()
	previous := model.ObservedState{Pools: []model.PoolObservation{
		{ID: "light", MaxCapacity: 12, CapacityAcknowledged: true},
		{ID: "build", MaxCapacity: 0, CapacityAcknowledged: true},
	}}

	first := sequenceCapacityTransfer(previous, map[string]int{"light": 10, "build": 2})
	if want := map[string]int{"light": 10, "build": 0}; !reflect.DeepEqual(first, want) {
		t.Fatalf("first transfer phase = %#v, want %#v", first, want)
	}

	previous.Pools[0].MaxCapacity = 10
	second := sequenceCapacityTransfer(previous, map[string]int{"light": 10, "build": 2})
	if want := map[string]int{"light": 10, "build": 2}; !reflect.DeepEqual(second, want) {
		t.Fatalf("second transfer phase = %#v, want %#v", second, want)
	}
}

func TestCapacityTransferHoldsIncreaseUntilUnacknowledgedDecreaseCompletes(t *testing.T) {
	t.Parallel()
	previous := model.ObservedState{Pools: []model.PoolObservation{
		{ID: "light", MaxCapacity: 12, CapacityAcknowledged: false},
		{ID: "build", MaxCapacity: 0, CapacityAcknowledged: true},
	}}

	got := sequenceCapacityTransfer(previous, map[string]int{"light": 10, "build": 2})
	if want := map[string]int{"light": 10, "build": 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unacknowledged transfer phase = %#v, want %#v", got, want)
	}
}

func TestCapacityTransferAllowsIndependentIncreasesWithoutDecrease(t *testing.T) {
	t.Parallel()
	previous := model.ObservedState{Pools: []model.PoolObservation{
		{ID: "light", MaxCapacity: 8, CapacityAcknowledged: true},
		{ID: "build", MaxCapacity: 0, CapacityAcknowledged: true},
	}}

	got := sequenceCapacityTransfer(previous, map[string]int{"light": 8, "build": 2})
	if want := map[string]int{"light": 8, "build": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("independent increase = %#v, want %#v", got, want)
	}
}

func TestCapacityTransferSendsAllConcurrentDecreases(t *testing.T) {
	t.Parallel()
	previous := model.ObservedState{Pools: []model.PoolObservation{
		{ID: "light", MaxCapacity: 10, CapacityAcknowledged: true},
		{ID: "build", MaxCapacity: 2, CapacityAcknowledged: true},
	}}

	got := sequenceCapacityTransfer(previous, map[string]int{"light": 0, "build": 0})
	if want := map[string]int{"light": 0, "build": 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("concurrent decreases = %#v, want %#v", got, want)
	}
}
