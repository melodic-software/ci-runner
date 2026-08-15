package healthwatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/jobindex"
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
	snapshot JobsSnapshot
	err      error
}

func (f fakeJobsFile) Snapshot(context.Context) (JobsSnapshot, error) {
	if f.err != nil {
		return JobsSnapshot{}, f.err
	}
	return f.snapshot, nil
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
	checker := testChecker(t, store, fakeInventory{counts: map[string]int{"org": 2}}, fakeJobsFile{snapshot: JobsSnapshot{SizeBytes: 1024}}, &memorySidecar{}, now)

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

// TestCheckHealthyWhenJobsFilePinnedAtCapByCompactableRecords reproduces the
// designed post-compaction steady state: the file sits at the save cap, but
// every record is reclaimable, so the watchdog stays quiet.
func TestCheckHealthyWhenJobsFilePinnedAtCapByCompactableRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now,
	})
	tombstoned := now.Add(-time.Hour)
	checker := testChecker(
		t,
		store,
		fakeInventory{},
		fakeJobsFile{snapshot: JobsSnapshot{
			SizeBytes: jobindex.MaximumJobStateBytes,
			Catalog: jobindex.Catalog{SchemaVersion: 1, Records: []jobindex.Record{
				{PoolID: "org", RunnerName: "runner-a", CompletedAt: now.Add(-2 * time.Hour), UpdatedAt: now},
				{PoolID: "org", RunnerName: "runner-b", TombstonedAt: &tombstoned, UpdatedAt: now},
			}},
		}},
		&memorySidecar{},
		now,
	)

	result, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Healthy || len(result.Findings) != 0 {
		t.Fatalf("result = %#v, want healthy at designed cap saturation", result)
	}
}

func TestCheckFlagsJobsLiveSizeWarning(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now,
	})
	// Open records are never compactable; 80 records carrying 100kB job IDs
	// encode to ~8MB of live payload, past any threshold up to 95%.
	records := make([]jobindex.Record, 0, 80)
	for i := 0; i < 80; i++ {
		records = append(records, jobindex.Record{
			PoolID:     "org",
			RunnerName: fmt.Sprintf("runner-%05d", i),
			JobID:      fmt.Sprintf("%05d-%s", i, strings.Repeat("x", 100_000)),
			Open:       true,
			UpdatedAt:  now,
		})
	}
	checker := testChecker(
		t,
		store,
		fakeInventory{},
		fakeJobsFile{snapshot: JobsSnapshot{
			SizeBytes: jobindex.MaximumJobStateBytes,
			Catalog:   jobindex.Catalog{SchemaVersion: 1, Records: records},
		}},
		&memorySidecar{},
		now,
	)

	result, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Healthy || len(result.Findings) != 1 || result.Findings[0].Code != "jobs-live-size-warning" {
		t.Fatalf("result = %#v, want jobs live size warning", result)
	}
}

func TestCheckFlagsJobsSizeOverflow(t *testing.T) {
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
		fakeJobsFile{snapshot: JobsSnapshot{
			SizeBytes: jobindex.MaximumJobStateBytes + 1,
			Catalog:   jobindex.Catalog{SchemaVersion: 1},
		}},
		&memorySidecar{},
		now,
	)

	result, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Healthy || len(result.Findings) != 1 || result.Findings[0].Code != "jobs-size-overflow" {
		t.Fatalf("result = %#v, want jobs size overflow", result)
	}
}

func TestCheckFlagsUndecodableJobsFile(t *testing.T) {
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
		fakeJobsFile{err: fmt.Errorf("%w: decode jobs.json: unexpected EOF", ErrJobsFileUndecodable)},
		&memorySidecar{},
		now,
	)

	result, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Healthy || len(result.Findings) != 1 || result.Findings[0].Code != "jobs-index-undecodable" {
		t.Fatalf("result = %#v, want undecodable jobs index finding", result)
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

type fakeJobsBytes struct {
	contents []byte
	err      error
}

func (f fakeJobsBytes) SnapshotBytes(context.Context) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.contents, nil
}

func TestStoreJobsFileMissingIsIgnored(t *testing.T) {
	t.Parallel()
	file := StoreJobsFile{Source: fakeJobsBytes{err: jobindex.ErrNotFound}}
	if _, err := file.Snapshot(context.Background()); !errors.Is(err, ErrJobsFileMissing) {
		t.Fatalf("err = %v, want ErrJobsFileMissing", err)
	}
}

func TestStoreJobsFileSnapshotDecodesCatalog(t *testing.T) {
	t.Parallel()
	contents := []byte(`{"schemaVersion":1,"records":[{"poolId":"org","runnerName":"runner-a","updatedAt":"2026-08-12T12:00:00Z","open":true}]}` + "\n")
	snapshot, err := StoreJobsFile{Source: fakeJobsBytes{contents: contents}}.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SizeBytes != int64(len(contents)) || len(snapshot.Catalog.Records) != 1 || !snapshot.Catalog.Records[0].Open {
		t.Fatalf("snapshot = %#v, want one open record and the document size", snapshot)
	}
}

func TestStoreJobsFileSnapshotWrapsDecodeFailure(t *testing.T) {
	t.Parallel()
	file := StoreJobsFile{Source: fakeJobsBytes{contents: []byte("{ not json")}}
	if _, err := file.Snapshot(context.Background()); !errors.Is(err, ErrJobsFileUndecodable) {
		t.Fatalf("err = %v, want ErrJobsFileUndecodable", err)
	}
}
