package healthwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

type fakeInventory struct {
	counts map[string]int
	err    error
}

func (f fakeInventory) RunningByPool(context.Context, string) (map[string]int, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]int, len(f.counts))
	for key, value := range f.counts {
		out[key] = value
	}
	return out, nil
}

type fakeJobsFile struct {
	size int64
	err  error
}

func (f fakeJobsFile) Size(context.Context) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.size, nil
}

type memorySidecar struct {
	mu       sync.Mutex
	contents Sidecar
}

func (m *memorySidecar) Load(context.Context) (Sidecar, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.contents, nil
}

func (m *memorySidecar) Save(_ context.Context, sidecar Sidecar) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contents = sidecar
	return nil
}

func testChecker(t *testing.T, store state.Store, inventory WorkerInventory, jobs JobsFileStat, sidecar SidecarStore, now time.Time) *Checker {
	t.Helper()
	checker, err := NewChecker(Dependencies{
		Now: func() time.Time { return now },
		Config: config.Config{
			Host: config.Host{ID: "melo-desk-001"},
			Controller: config.Controller{
				ReconcileInterval: config.Duration{Duration: 5 * time.Second},
			},
			HealthWatchdog: config.HealthWatchdog{
				HeartbeatStaleMultiplier: 3,
				WorkerDivergenceGrace:    config.Duration{Duration: time.Minute},
				JobsSizeWarningPercent:   90,
				AlertCooldown:            config.Duration{Duration: 15 * time.Minute},
			},
		},
		Store:     store,
		Inventory: inventory,
		JobsFile:  jobs,
		Sidecar:   sidecar,
	})
	if err != nil {
		t.Fatal(err)
	}
	return checker
}

func TestCheckHealthyWhenSignalsAreFresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now.Add(-5 * time.Second),
		Pools: []model.PoolObservation{{
			ID: "org", DesiredWorkers: 2,
		}},
	})
	checker := testChecker(t, store, fakeInventory{counts: map[string]int{"org": 2}}, fakeJobsFile{size: 1024}, &memorySidecar{}, now)

	result, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Healthy || len(result.Findings) != 0 {
		t.Fatalf("result = %#v, want healthy", result)
	}
}

func TestCheckFlagsStaleHeartbeat(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now.Add(-30 * time.Second),
	})
	checker := testChecker(t, store, fakeInventory{}, fakeJobsFile{}, &memorySidecar{}, now)

	result, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Healthy || len(result.Findings) != 1 || result.Findings[0].Code != "stale-heartbeat" {
		t.Fatalf("result = %#v, want stale heartbeat finding", result)
	}
}

func TestCheckFlagsWorkerDivergenceAfterGrace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now,
		Pools: []model.PoolObservation{{
			ID: "org", DesiredWorkers: 3,
		}},
	})
	sidecar := &memorySidecar{contents: Sidecar{WorkerDivergenceSince: ptrTime(now.Add(-2 * time.Minute))}}
	checker := testChecker(t, store, fakeInventory{counts: map[string]int{"org": 1}}, fakeJobsFile{}, sidecar, now)

	result, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Healthy || len(result.Findings) != 1 || result.Findings[0].Code != "worker-divergence" {
		t.Fatalf("result = %#v, want worker divergence finding", result)
	}
}

func TestCheckFlagsJobsSizeWarning(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now,
	})
	checker := testChecker(
		t,
		store,
		fakeInventory{},
		fakeJobsFile{size: int64(jobsFileSafetyCapBytes * 90 / 100)},
		&memorySidecar{},
		now,
	)

	result, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Healthy || len(result.Findings) != 1 || result.Findings[0].Code != "jobs-size-warning" {
		t.Fatalf("result = %#v, want jobs size warning", result)
	}
}

func TestCheckPreservesFindingsWhenInventoryFails(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now.Add(-30 * time.Second),
	})
	checker := testChecker(t, store, fakeInventory{err: errors.New("docker unavailable")}, fakeJobsFile{}, &memorySidecar{}, now)

	result, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Healthy || len(result.Findings) < 2 {
		t.Fatalf("result = %#v, want stale heartbeat plus inventory finding", result)
	}
	codes := map[string]struct{}{}
	for _, finding := range result.Findings {
		codes[finding.Code] = struct{}{}
	}
	if _, ok := codes["stale-heartbeat"]; !ok {
		t.Fatalf("findings = %#v, want stale-heartbeat", result.Findings)
	}
	if _, ok := codes["inventory-unavailable"]; !ok {
		t.Fatalf("findings = %#v, want inventory-unavailable", result.Findings)
	}
}

func TestCheckAndAlertDedupesRepeatedFindings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now.Add(-30 * time.Second),
	})
	sidecar := &memorySidecar{}
	var alerts int
	checker, err := NewChecker(Dependencies{
		Now: func() time.Time { return now },
		Config: config.Config{
			Host: config.Host{ID: "melo-desk-001"},
			Controller: config.Controller{
				ReconcileInterval: config.Duration{Duration: 5 * time.Second},
			},
			HealthWatchdog: config.HealthWatchdog{
				HeartbeatStaleMultiplier: 3,
				WorkerDivergenceGrace:    config.Duration{Duration: time.Minute},
				JobsSizeWarningPercent:   90,
				AlertCooldown:            config.Duration{Duration: 15 * time.Minute},
			},
		},
		Store:     store,
		Inventory: fakeInventory{},
		JobsFile:  fakeJobsFile{},
		Sidecar:   sidecar,
		Alert: alertFunc(func(context.Context, Result) error {
			alerts++
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := checker.CheckAndAlert(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := checker.CheckAndAlert(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if alerts != 1 {
		t.Fatalf("alerts = %d, want deduplicated single alert", alerts)
	}
}

type alertFunc func(context.Context, Result) error

func (f alertFunc) Send(ctx context.Context, result Result) error { return f(ctx, result) }

func ptrTime(value time.Time) *time.Time { return &value }

func TestNewCheckerRequiresDependencies(t *testing.T) {
	t.Parallel()
	if _, err := NewChecker(Dependencies{}); err == nil {
		t.Fatal("expected missing dependency error")
	}
}

func TestOSJobsFileMissingIsIgnored(t *testing.T) {
	t.Parallel()
	file := OSJobsFile{Path: t.TempDir() + "/missing-jobs.json"}
	if _, err := file.Size(context.Background()); !errors.Is(err, ErrJobsFileMissing) {
		t.Fatalf("err = %v, want ErrJobsFileMissing", err)
	}
}
