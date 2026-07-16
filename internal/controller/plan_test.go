package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/model"
)

func TestDesiredFormulaAndStartCount(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Pools[0].TotalAssignedJobs = 2
	plan := BuildPlan(input)
	if got := plan.DesiredWorkers["org"]; got != 3 {
		t.Fatalf("desired workers = %d, want min(3, 2+1)=3", got)
	}
	if got := plan.AdvertisedCapacity["org"]; got != 3 {
		t.Fatalf("advertised capacity = %d, want 3", got)
	}
	if len(plan.Start) != 1 || plan.Start[0] != (StartDecision{PoolID: "org", Count: 3}) {
		t.Fatalf("start = %#v", plan.Start)
	}
}

func TestIdlePoolAdvertisesFullServiceCapacityWithoutPrestartingBacklogWorkers(t *testing.T) {
	t.Parallel()
	input := healthyInput()

	plan := BuildPlan(input)

	if plan.DesiredWorkers["org"] != 1 || totalStarts(plan.Start) != 1 {
		t.Fatalf("desired=%d starts=%#v, want only one warm worker", plan.DesiredWorkers["org"], plan.Start)
	}
	if plan.AdvertisedCapacity["org"] != 3 {
		t.Fatalf("advertised capacity=%d, want resource-bounded target maximum 3", plan.AdvertisedCapacity["org"])
	}
}

func TestScaleToZeroPoolAdvertisesServiceCapacityWithoutPrestartingWorkers(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.GitHub.Targets[0].WarmIdle = 0

	plan := BuildPlan(input)

	if plan.DesiredWorkers["org"] != 0 || len(plan.Start) != 0 {
		t.Fatalf("desired=%d starts=%#v, want scale-to-zero inventory", plan.DesiredWorkers["org"], plan.Start)
	}
	if plan.AdvertisedCapacity["org"] != 3 {
		t.Fatalf("advertised capacity=%d, want jobs admitted up to target maximum 3", plan.AdvertisedCapacity["org"])
	}
}

func TestAdvertisedServiceCapacityRemainsMemoryBounded(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.Resources.MaximumConcurrentWorkers = 6
	input.Config.GitHub.Targets[0].MaxCapacity = 6
	input.Resources.AvailableMemoryBytes = 40 << 30 // 24 GiB above the floor funds three 8-GiB workers total.

	plan := BuildPlan(input)

	if plan.DesiredWorkers["org"] != 1 || totalStarts(plan.Start) != 1 {
		t.Fatalf("desired=%d starts=%#v, want only one warm worker", plan.DesiredWorkers["org"], plan.Start)
	}
	if plan.AdvertisedCapacity["org"] != 2 {
		t.Fatalf("advertised capacity=%d, want two slots before the upward memory margin", plan.AdvertisedCapacity["org"])
	}
}

func TestAdvertisedCapacityUsesMemorySchmittTriggerAtEverySlotBoundary(t *testing.T) {
	t.Parallel()
	const (
		gibibyte    = uint64(1 << 30)
		memoryFloor = 16 * gibibyte
	)
	tests := []struct {
		name             string
		previousCapacity int
		headroomBytes    uint64
		wantCapacity     int
	}{
		{name: "zero to one waits below upper boundary", headroomBytes: 10*gibibyte - 1, wantCapacity: 0},
		{name: "zero to one grows at upper boundary", headroomBytes: 10 * gibibyte, wantCapacity: 1},
		{name: "existing slot holds at lower boundary", previousCapacity: 1, headroomBytes: 8 * gibibyte, wantCapacity: 1},
		{name: "existing slot drops below lower boundary", previousCapacity: 1, headroomBytes: 8*gibibyte - 1, wantCapacity: 0},
		{name: "multi-slot decrease is immediate", previousCapacity: 3, headroomBytes: 16*gibibyte - 1, wantCapacity: 1},
		{name: "second slot waits for its upper boundary", previousCapacity: 1, headroomBytes: 18*gibibyte - 1, wantCapacity: 1},
		{name: "second slot grows at its upper boundary", previousCapacity: 1, headroomBytes: 18 * gibibyte, wantCapacity: 2},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := healthyInput()
			input.Config.GitHub.Targets[0].WarmIdle = 0
			input.Resources.AvailableMemoryBytes = memoryFloor + test.headroomBytes
			input.Previous.Pools = []model.PoolObservation{{
				ID: "org", MaxCapacity: test.previousCapacity, CapacityAcknowledged: true,
			}}

			plan := BuildPlan(input)

			if got := plan.AdvertisedCapacity["org"]; got != test.wantCapacity {
				t.Fatalf("advertised capacity = %d, want %d", got, test.wantCapacity)
			}
		})
	}
}

func TestAdvertisedCapacityMarginUsesTargetEffectiveWorkerMemory(t *testing.T) {
	t.Parallel()
	const gibibyte = uint64(1 << 30)
	workerMemory := config.ByteSize(4 * gibibyte)
	input := healthyInput()
	input.Config.GitHub.Targets[0].WarmIdle = 0
	input.Config.GitHub.Targets[0].Resources.Worker = &config.WorkerOverrides{Memory: &workerMemory}
	input.Resources.AvailableMemoryBytes = 16*gibibyte + 5*gibibyte - 1

	below := BuildPlan(input)
	if got := below.AdvertisedCapacity["org"]; got != 0 {
		t.Fatalf("capacity below profile-specific upper boundary = %d, want 0", got)
	}

	input.Resources.AvailableMemoryBytes++
	atBoundary := BuildPlan(input)
	if got := atBoundary.AdvertisedCapacity["org"]; got != 1 {
		t.Fatalf("capacity at profile-specific upper boundary = %d, want 1", got)
	}
}

func TestAdvertisedCapacityPendingHysteresisPreservesImmediateMultiSlotDecrease(t *testing.T) {
	t.Parallel()
	const gibibyte = uint64(1 << 30)
	input := healthyInput()
	input.Config.GitHub.Targets[0].WarmIdle = 0
	input.Previous.Pools = []model.PoolObservation{{
		ID: "org", MaxCapacity: 0, CapacityAcknowledged: false,
	}}
	input.CapacityHysteresis = map[string]int{"org": 3}
	// Three slots are in flight, but current headroom safely funds only one.
	// The lower raw boundary must win over the pending hysteresis baseline.
	input.Resources.AvailableMemoryBytes = 16*gibibyte + 2*8*gibibyte - 1

	plan := BuildPlan(input)

	if got := plan.AdvertisedCapacity["org"]; got != 1 {
		t.Fatalf("capacity after pending multi-slot withdrawal = %d, want 1", got)
	}
}

func TestGlobalCapacityReservesAssignmentsAcrossPoolsBeforeWarmIdle(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.Resources.MaximumConcurrentWorkers = 2
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets,
		config.Target{ID: "personal", MaxCapacity: 2, WarmIdle: 1, Priority: 10},
	)
	input.Pools[0].TotalAssignedJobs = 1
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "personal", TotalAssignedJobs: 1, Ready: true})
	plan := BuildPlan(input)
	if got := plan.DesiredWorkers["org"]; got != 1 {
		t.Fatalf("org desired = %d, want its assigned worker without optional warm idle", got)
	}
	if got := plan.DesiredWorkers["personal"]; got != 1 {
		t.Fatalf("personal desired = %d, want its assignment reserved before priority", got)
	}
	if totalStarts(plan.Start) != 2 {
		t.Fatalf("starts = %#v", plan.Start)
	}
}

func TestAdvertisedMultiPoolCombinationRemainsGloballyServiceable(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets,
		config.Target{ID: "personal", MaxCapacity: 3, WarmIdle: 1, Priority: 10},
	)
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "personal", Ready: true})

	idle := BuildPlan(input)
	if idle.DesiredWorkers["org"] != 1 || idle.DesiredWorkers["personal"] != 1 ||
		idle.AdvertisedCapacity["org"] != 2 || idle.AdvertisedCapacity["personal"] != 1 {
		t.Fatalf("idle allocation = desired %#v capacity %#v, want warm 1/1 and advertised 2/1", idle.DesiredWorkers, idle.AdvertisedCapacity)
	}

	input.Pools[0].TotalAssignedJobs = 2
	input.Pools[1].TotalAssignedJobs = 1
	assigned := BuildPlan(input)
	if assigned.DesiredWorkers["org"] != 2 || assigned.DesiredWorkers["personal"] != 1 {
		t.Fatalf("assigned desired = %#v, want every advertised obligation serviceable", assigned.DesiredWorkers)
	}
	if assigned.AdvertisedCapacity["org"] != 2 || assigned.AdvertisedCapacity["personal"] != 1 || totalStarts(assigned.Start) != 3 {
		t.Fatalf("assigned plan = starts %#v capacity %#v", assigned.Start, assigned.AdvertisedCapacity)
	}

	input.Pools[0].TotalAssignedJobs = 1
	input.Pools[1].TotalAssignedJobs = 2
	reversed := BuildPlan(input)
	if reversed.DesiredWorkers["org"] != 1 || reversed.DesiredWorkers["personal"] != 2 || totalStarts(reversed.Start) != 3 {
		t.Fatalf("reversed assigned plan = desired %#v starts %#v", reversed.DesiredWorkers, reversed.Start)
	}
}

func TestMultiPoolAssignmentsRemainReservedWhileResourceGateAdvertisesZero(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets,
		config.Target{ID: "personal", MaxCapacity: 3, WarmIdle: 1, Priority: 10},
	)
	input.Pools[0].TotalAssignedJobs = 2
	input.Pools[0].DrainServiceCapacity = 2
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "personal", TotalAssignedJobs: 1, DrainServiceCapacity: 1, Ready: true})
	input.Resources = model.ResourceSnapshot{}

	plan := BuildPlan(input)
	if plan.AdvertisedCapacity["org"] != 0 || plan.AdvertisedCapacity["personal"] != 0 {
		t.Fatalf("resource gate reopened capacity: %#v", plan.AdvertisedCapacity)
	}
	if plan.DesiredWorkers["org"] != 2 || plan.DesiredWorkers["personal"] != 1 || totalStarts(plan.Start) != 3 {
		t.Fatalf("resource gate stranded assignments: desired %#v starts %#v", plan.DesiredWorkers, plan.Start)
	}
}

func TestAssignmentReservationNeverExceedsPoolMaximum(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Pools[0].TotalAssignedJobs = 5

	plan := BuildPlan(input)

	if plan.DesiredWorkers["org"] != 3 || totalStarts(plan.Start) != 3 {
		t.Fatalf("over-cap assignment reservation = desired %d starts %#v, want pool maximum 3", plan.DesiredWorkers["org"], plan.Start)
	}
	if plan.AdvertisedCapacity["org"] != 0 {
		t.Fatalf("over-cap assignment reopened listener capacity: %#v", plan.AdvertisedCapacity)
	}
	if plan.Phase != model.PhaseDegraded || !hasProblem(plan.Problems, "assigned-job-count-exceeds-pool-cap", "org") {
		t.Fatalf("over-cap assignment was not explicit and degraded: phase=%s problems=%#v", plan.Phase, plan.Problems)
	}
}

func TestOverCapPoolCannotConsumeAnotherPoolsAssignmentReservation(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.GitHub.Targets[0].MaxCapacity = 2
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets,
		config.Target{ID: "personal", MaxCapacity: 2, WarmIdle: 1, Priority: 10},
	)
	input.Pools[0].TotalAssignedJobs = 5
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "personal", TotalAssignedJobs: 1, Ready: true})

	plan := BuildPlan(input)

	if plan.DesiredWorkers["org"] != 2 || plan.DesiredWorkers["personal"] != 1 || totalStarts(plan.Start) != 3 {
		t.Fatalf("cross-pool over-cap reservation = desired %#v starts %#v, want capped 2 plus assigned 1", plan.DesiredWorkers, plan.Start)
	}
	if plan.AdvertisedCapacity["org"] != 0 || plan.AdvertisedCapacity["personal"] != 1 {
		t.Fatalf("cross-pool over-cap capacities = %#v, want offending pool zero and healthy pool one", plan.AdvertisedCapacity)
	}
	if !hasProblem(plan.Problems, "assigned-job-count-exceeds-pool-cap", "org") {
		t.Fatalf("cross-pool excess assignment problem missing: %#v", plan.Problems)
	}
}

func TestBusyWorkersAbovePoolMaximumArePreservedWithoutAdditionalStarts(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.GitHub.Targets[0].MaxCapacity = 2
	input.Pools[0].TotalAssignedJobs = 4
	input.Workers = []model.Worker{
		{ID: "busy-1", PoolID: "org", State: model.WorkerBusy},
		{ID: "busy-2", PoolID: "org", State: model.WorkerBusy},
		{ID: "busy-3", PoolID: "org", State: model.WorkerBusy},
	}

	plan := BuildPlan(input)

	if plan.DesiredWorkers["org"] != 3 || len(plan.Start) != 0 || plan.AdvertisedCapacity["org"] != 0 {
		t.Fatalf("busy over-cap preservation = desired %d starts %#v capacity %d", plan.DesiredWorkers["org"], plan.Start, plan.AdvertisedCapacity["org"])
	}
	assertNotRemoved(t, plan.Remove, "busy-1", "busy-2", "busy-3")
}

func TestDisabledDrainNeverRemovesBusyWorker(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Desired.Mode = model.ModeDisabled
	input.Workers = []model.Worker{
		{ID: "busy", PoolID: "org", State: model.WorkerBusy, JobID: "job-1"},
		{ID: "idle", PoolID: "org", State: model.WorkerIdle},
		{ID: "starting", PoolID: "org", State: model.WorkerStarting},
		{ID: "unregistered", PoolID: "org", State: model.WorkerUnregistered},
	}
	plan := BuildPlan(input)
	if plan.Phase != model.PhaseDraining {
		t.Fatalf("phase = %s", plan.Phase)
	}
	assertRemoved(t, plan.Remove, "idle", "starting", "unregistered")
	assertNotRemoved(t, plan.Remove, "busy")
	if plan.AdvertisedCapacity["org"] != 0 {
		t.Fatal("drain did not advertise zero")
	}
}

func TestGamingPlansUnregisteredWorkerRemoval(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Desired.Mode = model.ModeGaming
	input.Desktop.RunningWSLCount = 1
	input.Workers = []model.Worker{{ID: "unregistered", PoolID: "org", State: model.WorkerUnregistered}}

	plan := BuildPlan(input)
	assertRemoved(t, plan.Remove, "unregistered")
	if !plan.StopDesktop || !plan.ShutdownWSL {
		t.Fatalf("gaming cleanup did not proceed after planning unusable worker removal: %#v", plan)
	}
}

func TestGamingWaitsForBusyWorkBeforeDesktopShutdown(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Desired.Mode = model.ModeGaming
	input.Desktop.RunningWSLCount = 2
	input.Workers = []model.Worker{{ID: "busy", PoolID: "org", State: model.WorkerBusy}}
	plan := BuildPlan(input)
	if plan.StopDesktop || plan.ShutdownWSL {
		t.Fatalf("gaming shutdown started while work is busy: %#v", plan)
	}
	if plan.Phase != model.PhaseDraining {
		t.Fatalf("phase = %s", plan.Phase)
	}

	input.Workers = nil
	plan = BuildPlan(input)
	if !plan.StopDesktop || !plan.ShutdownWSL {
		t.Fatalf("gaming actions = stop:%v wsl:%v", plan.StopDesktop, plan.ShutdownWSL)
	}
	input.Desktop = model.DesktopStatus{}
	plan = BuildPlan(input)
	if plan.Phase != model.PhaseGaming {
		t.Fatalf("verified gaming phase = %s", plan.Phase)
	}
}

func TestACOnlySuspendsImmediatelyAndResumesAfterStableWindow(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.Power.Policy = config.PowerACOnly
	input.Config.Power.StableACWindow = config.Duration{Duration: 30 * time.Second}
	input.Power.ACConnected = false
	input.Workers = []model.Worker{
		{ID: "busy", PoolID: "org", State: model.WorkerBusy},
		{ID: "idle", PoolID: "org", State: model.WorkerIdle},
	}
	plan := BuildPlan(input)
	if plan.Phase != model.PhasePowerSuspended {
		t.Fatalf("phase = %s", plan.Phase)
	}
	if plan.AdvertisedCapacity["org"] != 0 {
		t.Fatalf("power-suspended capacity = %d, want fail-closed zero", plan.AdvertisedCapacity["org"])
	}
	assertRemoved(t, plan.Remove, "idle")
	assertNotRemoved(t, plan.Remove, "busy")

	input.Power.ACConnected = true
	input.Workers = nil
	input.Previous.PowerGate = plan.PowerGate
	plan = BuildPlan(input)
	if plan.Phase != model.PhasePowerSuspended {
		t.Fatalf("first AC observation resumed immediately: %s", plan.Phase)
	}
	input.Previous.PowerGate = plan.PowerGate
	input.Now = input.Now.Add(29 * time.Second)
	plan = BuildPlan(input)
	if plan.Phase != model.PhasePowerSuspended {
		t.Fatal("resumed before stable AC window")
	}
	input.Previous.PowerGate = plan.PowerGate
	input.Now = input.Now.Add(time.Second)
	plan = BuildPlan(input)
	if plan.Phase != model.PhaseReady {
		t.Fatalf("phase after stable AC = %s", plan.Phase)
	}
}

func TestCPUBlockAndResumeHysteresis(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Resources.CPUUtilizationPercent = 80
	plan := BuildPlan(input)
	if plan.Phase != model.PhaseReady {
		t.Fatalf("CPU blocked without observation window: %s", plan.Phase)
	}
	input.Previous.ResourceGate = plan.ResourceGate
	input.Now = input.Now.Add(60 * time.Second)
	plan = BuildPlan(input)
	if plan.Phase != model.PhaseResourceConstrained || !plan.ResourceGate.Blocked || plan.ResourceGate.Reason != model.ResourceGateReasonCPU {
		t.Fatalf("CPU did not block after observation window: %#v", plan.ResourceGate)
	}
	if plan.AdvertisedCapacity["org"] != 0 {
		t.Fatalf("resource-constrained capacity = %d, want fail-closed zero", plan.AdvertisedCapacity["org"])
	}

	input.Resources.CPUUtilizationPercent = 60
	input.Previous.ResourceGate = plan.ResourceGate
	plan = BuildPlan(input)
	if plan.Phase != model.PhaseResourceConstrained {
		t.Fatal("blocked gate resumed without hysteresis")
	}
	input.Previous.ResourceGate = plan.ResourceGate
	input.Now = input.Now.Add(59 * time.Second)
	plan = BuildPlan(input)
	if plan.Phase != model.PhaseResourceConstrained {
		t.Fatal("blocked gate resumed before hysteresis window")
	}
	input.Previous.ResourceGate = plan.ResourceGate
	input.Now = input.Now.Add(time.Second)
	plan = BuildPlan(input)
	if plan.Phase != model.PhaseReady || plan.ResourceGate != (model.ResourceGateState{}) {
		t.Fatalf("gate did not resume: %#v", plan.ResourceGate)
	}
}

func TestLegacyMemoryBlockedGateClearsImmediatelyOnValidLowCPU(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Previous.ResourceGate = model.ResourceGateState{Blocked: true} // v0.1.7 persisted state had no reason.
	input.Resources.AvailableMemoryBytes = 20 << 30
	input.Workers = []model.Worker{{ID: "idle", PoolID: "org", State: model.WorkerIdle}}

	plan := BuildPlan(input)
	if plan.Phase != model.PhaseReady || plan.ResourceGate != (model.ResourceGateState{}) {
		t.Fatalf("legacy low-CPU resource gate was not cleared immediately: %#v", plan.ResourceGate)
	}
	if len(plan.Remove) != 0 || plan.AdvertisedCapacity["org"] != 1 {
		t.Fatalf("legacy memory block disturbed live capacity: %#v", plan)
	}
}

func TestLegacyBlockedGateMigratesToCPUAndRequiresResumeHysteresis(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Previous.ResourceGate = model.ResourceGateState{Blocked: true}
	input.Resources.CPUUtilizationPercent = 61

	plan := BuildPlan(input)
	if plan.Phase != model.PhaseResourceConstrained || plan.ResourceGate.Reason != model.ResourceGateReasonCPU || plan.ResourceGate.HealthySince != nil {
		t.Fatalf("legacy high-CPU gate migration = %#v", plan.ResourceGate)
	}

	input.Previous.ResourceGate = plan.ResourceGate
	input.Resources.CPUUtilizationPercent = 60
	plan = BuildPlan(input)
	if plan.Phase != model.PhaseResourceConstrained || plan.ResourceGate.HealthySince == nil {
		t.Fatalf("migrated CPU gate skipped resume hysteresis: %#v", plan.ResourceGate)
	}
	input.Previous.ResourceGate = plan.ResourceGate
	input.Now = input.Now.Add(60 * time.Second)
	plan = BuildPlan(input)
	if plan.Phase != model.PhaseReady || plan.ResourceGate != (model.ResourceGateState{}) {
		t.Fatalf("migrated CPU gate did not recover: %#v", plan.ResourceGate)
	}
}

func TestInvalidObservationReasonRecoversOnlyAfterHealthyHysteresis(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Resources.TotalMemoryBytes = 0

	plan := BuildPlan(input)
	if plan.Phase != model.PhaseResourceConstrained || plan.ResourceGate.Reason != model.ResourceGateReasonInvalidObservation {
		t.Fatalf("invalid observation gate = %#v", plan.ResourceGate)
	}

	input.Previous.ResourceGate = plan.ResourceGate
	input.Resources = healthyInput().Resources
	plan = BuildPlan(input)
	if plan.Phase != model.PhaseResourceConstrained || plan.ResourceGate.HealthySince == nil || plan.ResourceGate.Reason != model.ResourceGateReasonInvalidObservation {
		t.Fatalf("invalid-observation gate recovered without hysteresis: %#v", plan.ResourceGate)
	}
	input.Previous.ResourceGate = plan.ResourceGate
	input.Now = input.Now.Add(60 * time.Second)
	plan = BuildPlan(input)
	if plan.Phase != model.PhaseReady || plan.ResourceGate != (model.ResourceGateState{}) {
		t.Fatalf("invalid-observation gate did not clear after healthy hysteresis: %#v", plan.ResourceGate)
	}
}

func TestResourceGateStateLegacyJSONBackwardCompatibility(t *testing.T) {
	t.Parallel()
	var state model.ResourceGateState
	if err := json.Unmarshal([]byte(`{"blocked":true}`), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Blocked || state.Reason != "" {
		t.Fatalf("legacy resource gate = %#v", state)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"reason"`)) {
		t.Fatalf("legacy empty reason was not omitted: %s", encoded)
	}
}

func TestResourceGateStateReasonJSONRoundTrip(t *testing.T) {
	t.Parallel()
	want := model.ResourceGateState{Blocked: true, Reason: model.ResourceGateReasonInvalidObservation}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got model.ResourceGateState
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resource gate round trip = %#v, want %#v", got, want)
	}
}

func TestLowMemoryRetainsIdleWarmCapacityWithoutBlockingGate(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Resources.AvailableMemoryBytes = 20 << 30 // Only 4 GiB remains above the 25% floor.
	input.Workers = []model.Worker{{ID: "idle", PoolID: "org", State: model.WorkerIdle}}
	plan := BuildPlan(input)
	if plan.Phase != model.PhaseReady || plan.ResourceGate.Blocked || plan.ResourceGate.HealthySince != nil {
		t.Fatalf("low-memory gate = %#v phase=%s", plan.ResourceGate, plan.Phase)
	}
	if len(plan.Start) != 0 || len(plan.Remove) != 0 || plan.AdvertisedCapacity["org"] != 1 || plan.DesiredWorkers["org"] != 1 {
		t.Fatalf("low-memory plan disturbed satisfied warm capacity: %#v", plan)
	}
}

func TestLowMemoryWithNoPendingWorkDoesNotBlockGate(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.GitHub.Targets[0].WarmIdle = 0
	input.Resources.AvailableMemoryBytes = 20 << 30

	plan := BuildPlan(input)
	if plan.Phase != model.PhaseReady || plan.ResourceGate != (model.ResourceGateState{}) || len(plan.Start) != 0 {
		t.Fatalf("no-pending low-memory plan = %#v", plan)
	}
}

func TestUnaffordableLargeTargetPreservesExistingSmallerPool(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.GitHub.Targets[0].Priority = 10
	input.Workers = []model.Worker{{ID: "ordinary-idle", PoolID: "org", State: model.WorkerIdle}}
	highMemory := config.ByteSize(24 << 30)
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets, config.Target{
		ID: "codeql", MaxCapacity: 1, WarmIdle: 1, Priority: 0,
		Resources: config.TargetResources{Worker: &config.WorkerOverrides{Memory: &highMemory, MemorySwap: &highMemory}},
	})
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "codeql", Ready: true})
	input.Resources.AvailableMemoryBytes = 20 << 30

	plan := BuildPlan(input)
	if plan.Phase != model.PhaseReady || plan.ResourceGate.Blocked || len(plan.Start) != 0 || len(plan.Remove) != 0 {
		t.Fatalf("unaffordable large-target plan = %#v", plan)
	}
	if plan.AdvertisedCapacity["org"] != 1 || plan.AdvertisedCapacity["codeql"] != 0 {
		t.Fatalf("capacity = %#v, want existing ordinary capacity only", plan.AdvertisedCapacity)
	}
}

func TestLowMemoryNeverRetiresBusyWorker(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Resources.AvailableMemoryBytes = 16 << 30
	input.Workers = []model.Worker{{ID: "busy", PoolID: "org", State: model.WorkerBusy}}

	plan := BuildPlan(input)
	if plan.Phase != model.PhaseReady || plan.ResourceGate.Blocked || len(plan.Start) != 0 {
		t.Fatalf("busy low-memory plan = %#v", plan)
	}
	assertNotRemoved(t, plan.Remove, "busy")
	if plan.AdvertisedCapacity["org"] != 1 {
		t.Fatalf("busy capacity = %d, want 1", plan.AdvertisedCapacity["org"])
	}
}

func TestDormantLargeTargetDoesNotReduceOrdinaryAdmission(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	highMemory := config.ByteSize(24 << 30)
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets, config.Target{
		ID: "codeql", MaxCapacity: 1, Priority: 10,
		Resources: config.TargetResources{Worker: &config.WorkerOverrides{Memory: &highMemory, MemorySwap: &highMemory}},
	})
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "codeql", Ready: true})
	input.Resources.AvailableMemoryBytes = 24 << 30 // Exactly one ordinary 8-GiB worker above the floor.

	plan := BuildPlan(input)
	if len(plan.Start) != 1 || plan.Start[0] != (StartDecision{PoolID: "org", Count: 1}) {
		t.Fatalf("starts = %#v, want the ordinary worker", plan.Start)
	}
	if plan.AdvertisedCapacity["org"] != 1 || plan.AdvertisedCapacity["codeql"] != 0 {
		t.Fatalf("advertised capacity = %#v", plan.AdvertisedCapacity)
	}
}

func TestLargeTargetRequiresItsOwnMemoryHeadroom(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.GitHub.Targets[0].WarmIdle = 0
	highMemory := config.ByteSize(24 << 30)
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets, config.Target{
		ID: "codeql", MaxCapacity: 1, WarmIdle: 1,
		Resources: config.TargetResources{Worker: &config.WorkerOverrides{Memory: &highMemory, MemorySwap: &highMemory}},
	})
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "codeql", Ready: true})
	input.Resources.AvailableMemoryBytes = 39 << 30 // 23 GiB above the floor cannot fund CodeQL.

	plan := BuildPlan(input)
	if plan.Phase != model.PhaseReady || plan.ResourceGate.Blocked || totalStarts(plan.Start) != 0 {
		t.Fatalf("underfunded plan = %#v", plan)
	}

	input.Resources.AvailableMemoryBytes = 40 << 30
	plan = BuildPlan(input)
	if len(plan.Start) != 1 || plan.Start[0] != (StartDecision{PoolID: "codeql", Count: 1}) {
		t.Fatalf("exactly funded starts = %#v", plan.Start)
	}
}

func TestMixedProfilesChargeExactMemory(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.Resources.MaximumConcurrentWorkers = 2
	highMemory := config.ByteSize(24 << 30)
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets, config.Target{
		ID: "codeql", MaxCapacity: 1, WarmIdle: 1, Priority: 10,
		Resources: config.TargetResources{Worker: &config.WorkerOverrides{Memory: &highMemory, MemorySwap: &highMemory}},
	})
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "codeql", Ready: true})
	input.Resources.AvailableMemoryBytes = 48 << 30 // Exactly 8+24 GiB above the floor.

	plan := BuildPlan(input)
	if len(plan.Start) != 2 || plan.Start[0] != (StartDecision{PoolID: "org", Count: 1}) || plan.Start[1] != (StartDecision{PoolID: "codeql", Count: 1}) {
		t.Fatalf("starts = %#v, want exact 8+24-GiB allocation", plan.Start)
	}
}

func TestOversizedHighPriorityTargetDoesNotStarveSmallerTarget(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.Resources.MaximumConcurrentWorkers = 2
	input.Config.GitHub.Targets[0].Priority = 10
	highMemory := config.ByteSize(24 << 30)
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets, config.Target{
		ID: "codeql", MaxCapacity: 1, WarmIdle: 1, Priority: 0,
		Resources: config.TargetResources{Worker: &config.WorkerOverrides{Memory: &highMemory, MemorySwap: &highMemory}},
	})
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "codeql", Ready: true})
	input.Resources.AvailableMemoryBytes = 24 << 30 // Only the ordinary 8-GiB profile fits.

	plan := BuildPlan(input)
	if len(plan.Start) != 1 || plan.Start[0] != (StartDecision{PoolID: "org", Count: 1}) {
		t.Fatalf("starts = %#v, want smaller lower-priority target", plan.Start)
	}
}

func TestOutstandingAssignmentsUseWeightedMemoryAdmission(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Desired.Mode = model.ModeDisabled
	input.Config.GitHub.Targets[0].Priority = 10
	input.Pools[0].TotalAssignedJobs = 1
	input.Pools[0].DrainServiceCapacity = 1
	highMemory := config.ByteSize(24 << 30)
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets, config.Target{
		ID: "codeql", MaxCapacity: 1, Priority: 0,
		Resources: config.TargetResources{Worker: &config.WorkerOverrides{Memory: &highMemory, MemorySwap: &highMemory}},
	})
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "codeql", TotalAssignedJobs: 1, DrainServiceCapacity: 1, Ready: true})
	input.Resources.AvailableMemoryBytes = 24 << 30

	plan := BuildPlan(input)
	if len(plan.Start) != 1 || plan.Start[0] != (StartDecision{PoolID: "org", Count: 1}) {
		t.Fatalf("outstanding starts = %#v, want the smaller service obligation", plan.Start)
	}
}

func TestUniformProfilesRetainSlotCompatibility(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.Resources.MaximumConcurrentWorkers = 3
	input.Config.GitHub.Targets[0].WarmIdle = 2
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets, config.Target{
		ID: "personal", MaxCapacity: 1, WarmIdle: 1, Priority: 10,
	})
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "personal", Ready: true})
	input.Resources.AvailableMemoryBytes = 40 << 30 // Three uniform 8-GiB profiles above the floor.

	plan := BuildPlan(input)
	if totalStarts(plan.Start) != 3 || plan.AdvertisedCapacity["org"] != 2 || plan.AdvertisedCapacity["personal"] != 1 {
		t.Fatalf("uniform allocation changed: starts=%#v capacity=%#v", plan.Start, plan.AdvertisedCapacity)
	}
}

func TestCapacityOverrideZeroIsDataOnlyPause(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	zero := 0
	input.Desired.TemporaryCapacityOverride = &zero
	input.Workers = []model.Worker{{ID: "idle", PoolID: "org", State: model.WorkerIdle}}
	plan := BuildPlan(input)
	if plan.AdvertisedCapacity["org"] != 0 || plan.DesiredWorkers["org"] != 0 {
		t.Fatalf("capacity = %#v desired = %#v", plan.AdvertisedCapacity, plan.DesiredWorkers)
	}
	assertRemoved(t, plan.Remove, "idle")
}

func TestEnabledPoolKeepsSingleExcessIdleWorkerAvailableForNaturalConvergence(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.Resources.MaximumConcurrentWorkers = 12
	input.Config.Resources.Worker.Memory = config.ByteSize(2 << 30)
	input.Config.GitHub.Targets[0].MaxCapacity = 12
	input.Config.GitHub.Targets[0].WarmIdle = 3
	input.Workers = []model.Worker{
		{ID: "idle-1", PoolID: "org", State: model.WorkerIdle},
		{ID: "idle-2", PoolID: "org", State: model.WorkerIdle},
		{ID: "idle-3", PoolID: "org", State: model.WorkerIdle},
		{ID: "idle-4", PoolID: "org", State: model.WorkerIdle},
	}

	plan := BuildPlan(input)

	if plan.DesiredWorkers["org"] != 3 || plan.AdvertisedCapacity["org"] != 12 {
		t.Fatalf("desired=%d capacity=%d, want three warm workers and uninterrupted service capacity", plan.DesiredWorkers["org"], plan.AdvertisedCapacity["org"])
	}
	if plan.Phase != model.PhaseReady {
		t.Fatalf("phase = %s, want ready", plan.Phase)
	}
	assertNotRemoved(t, plan.Remove, "idle-1", "idle-2", "idle-3", "idle-4")
}

func TestSingleExcessBurstInventorySharesOneGlobalCapacityBudget(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.Resources.MaximumConcurrentWorkers = 12
	input.Config.Resources.Worker.Memory = config.ByteSize(2 << 30)
	input.Config.GitHub.Targets[0] = config.Target{ID: "light", MaxCapacity: 12, WarmIdle: 11, Priority: 0}
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets,
		config.Target{ID: "build", MaxCapacity: 12, WarmIdle: 0, Priority: 10},
	)
	input.Pools = []PoolSnapshot{{TargetID: "light", Ready: true}, {TargetID: "build", Ready: true}}
	for index := range 12 {
		input.Workers = append(input.Workers, model.Worker{ID: fmt.Sprintf("light-%d", index), PoolID: "light", State: model.WorkerIdle})
	}
	input.Workers = append(input.Workers, model.Worker{ID: "build-0", PoolID: "build", State: model.WorkerIdle})

	plan := BuildPlan(input)

	if plan.DesiredWorkers["light"] != 11 || plan.DesiredWorkers["build"] != 0 {
		t.Fatalf("desired workers = %#v, want 11/0", plan.DesiredWorkers)
	}
	if plan.AdvertisedCapacity["light"] != 11 || plan.AdvertisedCapacity["build"] != 1 {
		t.Fatalf("advertised capacity = %#v, want zero-warm pool preserved inside 11/1 global budget", plan.AdvertisedCapacity)
	}
	if totalAdvertisedCapacity(plan.AdvertisedCapacity) > input.Config.Resources.MaximumConcurrentWorkers {
		t.Fatalf("advertised capacity %#v exceeds host limit %d", plan.AdvertisedCapacity, input.Config.Resources.MaximumConcurrentWorkers)
	}
	assertNotRemoved(t, plan.Remove, "build-0", "light-0")
}

func TestWarmSevenAcrossTwoPoolsNeverAdvertisesPastHostLimit(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.Resources.MaximumConcurrentWorkers = 12
	input.Config.Resources.Worker.Memory = config.ByteSize(2 << 30)
	input.Config.GitHub.Targets[0] = config.Target{ID: "light", MaxCapacity: 12, WarmIdle: 7, Priority: 0}
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets,
		config.Target{ID: "build", MaxCapacity: 12, WarmIdle: 7, Priority: 10},
	)
	input.Pools = []PoolSnapshot{{TargetID: "light", Ready: true}, {TargetID: "build", Ready: true}}
	for index := range 8 {
		input.Workers = append(input.Workers, model.Worker{ID: fmt.Sprintf("light-%d", index), PoolID: "light", State: model.WorkerIdle})
	}
	for index := range 6 {
		input.Workers = append(input.Workers, model.Worker{ID: fmt.Sprintf("build-%d", index), PoolID: "build", State: model.WorkerIdle})
	}

	plan := BuildPlan(input)

	if plan.DesiredWorkers["light"] != 7 || plan.DesiredWorkers["build"] != 5 {
		t.Fatalf("desired workers = %#v, want globally allocated warm inventory 7/5", plan.DesiredWorkers)
	}
	if got := totalAdvertisedCapacity(plan.AdvertisedCapacity); got != 12 {
		t.Fatalf("advertised capacity = %#v (sum %d), want host-bounded sum 12", plan.AdvertisedCapacity, got)
	}
	if plan.AdvertisedCapacity["light"] == 0 || plan.AdvertisedCapacity["build"] == 0 {
		t.Fatalf("one-worker retention blacked out a pool: %#v", plan.AdvertisedCapacity)
	}
}

func TestEnabledPoolQuiescesBeforeShrinkingMultipleExcessIdleWorkers(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Workers = []model.Worker{
		{ID: "idle-1", PoolID: "org", State: model.WorkerIdle},
		{ID: "idle-2", PoolID: "org", State: model.WorkerIdle},
		{ID: "idle-3", PoolID: "org", State: model.WorkerIdle},
	}
	plan := BuildPlan(input)
	if plan.DesiredWorkers["org"] != 1 || plan.AdvertisedCapacity["org"] != 0 {
		t.Fatalf("desired=%d capacity=%d, want one worker behind a zero-capacity quiesce", plan.DesiredWorkers["org"], plan.AdvertisedCapacity["org"])
	}
	if plan.Phase != model.PhaseDraining {
		t.Fatalf("phase = %s, want draining", plan.Phase)
	}
	if len(plan.Remove) != 2 {
		t.Fatalf("removals = %#v, want exactly two excess workers", plan.Remove)
	}
}

func TestBusyWorkersRemainWhenLimitDropsBelowBusyCount(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	one := 1
	input.Desired.TemporaryCapacityOverride = &one
	input.Workers = []model.Worker{
		{ID: "busy-1", PoolID: "org", State: model.WorkerBusy},
		{ID: "busy-2", PoolID: "org", State: model.WorkerBusy},
	}
	plan := BuildPlan(input)
	assertNotRemoved(t, plan.Remove, "busy-1", "busy-2")
	if plan.AdvertisedCapacity["org"] != 0 {
		t.Fatalf("capacity debt should advertise zero, got %d", plan.AdvertisedCapacity["org"])
	}
	if len(plan.Problems) == 0 || plan.Problems[0].Code != "capacity-below-busy-count" {
		t.Fatalf("problems = %#v", plan.Problems)
	}
}

func TestUnavailablePoolFailsClosedLocally(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Pools[0].Ready = false
	input.Workers = []model.Worker{{ID: "idle", PoolID: "org", State: model.WorkerIdle}}
	plan := BuildPlan(input)
	if plan.AdvertisedCapacity["org"] != 0 || len(plan.Start) != 0 {
		t.Fatalf("unavailable pool admitted work: %#v", plan)
	}
	assertRemoved(t, plan.Remove, "idle")
}

func TestUnavailableEmptyPoolNeverReceivesSpareCapacity(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Pools = nil

	plan := BuildPlan(input)

	if plan.AdvertisedCapacity["org"] != 0 || plan.DesiredWorkers["org"] != 0 || len(plan.Start) != 0 {
		t.Fatalf("unavailable empty pool admitted spare capacity: %#v", plan)
	}
	if len(plan.Problems) == 0 || plan.Problems[0].Code != "pool-unavailable" {
		t.Fatalf("unavailable pool problem = %#v", plan.Problems)
	}
}

func TestSpareCapacityIsAllocatedOnlyToReadyPools(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets,
		config.Target{ID: "unavailable", MaxCapacity: 3, WarmIdle: 1, Priority: -1},
	)
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "unavailable", Ready: false})

	plan := BuildPlan(input)

	if plan.AdvertisedCapacity["org"] != 3 || plan.DesiredWorkers["org"] != 1 {
		t.Fatalf("ready pool lost service capacity: desired %#v capacity %#v", plan.DesiredWorkers, plan.AdvertisedCapacity)
	}
	if plan.AdvertisedCapacity["unavailable"] != 0 || plan.DesiredWorkers["unavailable"] != 0 {
		t.Fatalf("unavailable higher-priority pool received capacity: desired %#v capacity %#v", plan.DesiredWorkers, plan.AdvertisedCapacity)
	}
}

func TestOrphanedBusyWorkerIsReportedAndPreserved(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Workers = []model.Worker{
		{ID: "orphan-busy", PoolID: "removed-pool", State: model.WorkerBusy},
		{ID: "orphan-idle", PoolID: "removed-pool", State: model.WorkerIdle},
	}
	plan := BuildPlan(input)
	assertNotRemoved(t, plan.Remove, "orphan-idle", "orphan-busy")
	if len(plan.Problems) == 0 || plan.Problems[0].Code != "orphaned-busy-worker" {
		t.Fatalf("problems = %#v", plan.Problems)
	}
}

func healthyInput() PlanInput {
	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	return PlanInput{
		Config: config.Config{
			SchemaVersion: config.SupportedSchemaVersion,
			Host:          config.Host{ID: "melo-desk-001", RunnerNamePrefix: "melo-desk-001"},
			GitHub:        config.GitHub{Targets: []config.Target{{ID: "org", MaxCapacity: 3, WarmIdle: 1, Priority: 0}}},
			Resources: config.Resources{
				MaximumConcurrentWorkers:        3,
				Worker:                          config.Worker{Memory: config.ByteSize(8 << 30)},
				MinimumAvailableMemoryPct:       25,
				MemoryCapacityIncreaseMarginPct: 25,
				CPUBlockPercent:                 75,
				CPUResumePercent:                60,
				CPUObservationWindow:            config.Duration{Duration: 60 * time.Second},
				CPUHysteresisWindow:             config.Duration{Duration: 60 * time.Second},
			},
			Power: config.Power{Policy: config.PowerAlways, StableACWindow: config.Duration{Duration: 30 * time.Second}},
		},
		Desired:   model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled},
		Pools:     []PoolSnapshot{{TargetID: "org", Ready: true}},
		Resources: model.ResourceSnapshot{TotalMemoryBytes: 64 << 30, AvailableMemoryBytes: 64 << 30, CPUUtilizationPercent: 10},
		Power:     model.PowerSnapshot{ACConnected: true, ObservedAt: now},
		Desktop:   model.DesktopStatus{DesktopRunning: true, EngineReachable: true},
		Now:       now,
	}
}

func totalStarts(decisions []StartDecision) int {
	total := 0
	for _, decision := range decisions {
		total += decision.Count
	}
	return total
}

func totalAdvertisedCapacity(capacities map[string]int) int {
	total := 0
	for _, capacity := range capacities {
		total += capacity
	}
	return total
}

func hasProblem(problems []model.Problem, code, poolID string) bool {
	for _, item := range problems {
		if item.Code == code && item.PoolID == poolID {
			return true
		}
	}
	return false
}

func assertRemoved(t *testing.T, workers []model.Worker, ids ...string) {
	t.Helper()
	found := make(map[string]bool, len(workers))
	for _, worker := range workers {
		found[worker.ID] = true
	}
	for _, id := range ids {
		if !found[id] {
			t.Errorf("worker %q was not removed; removals = %#v", id, workers)
		}
	}
}

func assertNotRemoved(t *testing.T, workers []model.Worker, ids ...string) {
	t.Helper()
	for _, worker := range workers {
		for _, id := range ids {
			if worker.ID == id {
				t.Errorf("busy worker %q was selected for removal", id)
			}
		}
	}
}
