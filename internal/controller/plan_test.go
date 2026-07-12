package controller

import (
	"bytes"
	"encoding/json"
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

func TestGlobalCapacityUsesPriorityAcrossPools(t *testing.T) {
	t.Parallel()
	input := healthyInput()
	input.Config.Resources.MaximumConcurrentWorkers = 2
	input.Config.GitHub.Targets = append(input.Config.GitHub.Targets,
		config.Target{ID: "personal", MaxCapacity: 2, WarmIdle: 1, Priority: 10},
	)
	input.Pools[0].TotalAssignedJobs = 1 // org wants two including its warm worker
	input.Pools = append(input.Pools, PoolSnapshot{TargetID: "personal", TotalAssignedJobs: 1, Ready: true})
	plan := BuildPlan(input)
	if got := plan.DesiredWorkers["org"]; got != 2 {
		t.Fatalf("org desired = %d, want 2", got)
	}
	if got := plan.DesiredWorkers["personal"]; got != 0 {
		t.Fatalf("personal desired = %d, want 0 after higher-priority allocation", got)
	}
	if totalStarts(plan.Start) != 2 {
		t.Fatalf("starts = %#v", plan.Start)
	}
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

func TestEnabledPoolQuiescesBeforeShrinkingExcessIdleWorkers(t *testing.T) {
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
			Host:   config.Host{ID: "melo-desk-001", RunnerNamePrefix: "melo-desk-001"},
			GitHub: config.GitHub{Targets: []config.Target{{ID: "org", MaxCapacity: 3, WarmIdle: 1, Priority: 0}}},
			Resources: config.Resources{
				MaximumConcurrentWorkers:  3,
				Worker:                    config.Worker{Memory: config.ByteSize(8 << 30)},
				MinimumAvailableMemoryPct: 25,
				CPUBlockPercent:           75,
				CPUResumePercent:          60,
				CPUObservationWindow:      config.Duration{Duration: 60 * time.Second},
				CPUHysteresisWindow:       config.Duration{Duration: 60 * time.Second},
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
