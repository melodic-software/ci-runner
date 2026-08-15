package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/healthwatch"
	"github.com/melodic-software/ci-runner/internal/jobindex"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/secret"
	"github.com/melodic-software/ci-runner/internal/state"
	statefs "github.com/melodic-software/ci-runner/internal/state/fs"
)

type healthWatchInventory struct {
	counts map[string]int
}

func (h healthWatchInventory) RunningByPool(context.Context, string) (map[string]int, error) {
	return h.counts, nil
}

func newHealthWatchJobsStore(t *testing.T) *jobindex.FileStore {
	t.Helper()
	directory := t.TempDir()
	locker, err := statefs.NewPlatformLocker(directory)
	if err != nil {
		t.Fatal(err)
	}
	store, err := jobindex.NewFileStore(directory, locker, secret.NewAccessController())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestHealthWatchCheckJSONReportsUnhealthy(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now.Add(-time.Minute),
	})
	var out, errOut bytes.Buffer
	application, err := New(Dependencies{
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
			Paths: config.Paths{State: t.TempDir()},
		},
		Store: store,
		Jobs:  newHealthWatchJobsStore(t),
		Now:   func() time.Time { return now },
	}, strings.NewReader(""), &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}

	originalNewChecker := initHealthWatchChecker
	initHealthWatchChecker = func(deps healthwatch.Dependencies) (*healthwatch.Checker, error) {
		deps.Inventory = healthWatchInventory{}
		deps.Sidecar = healthwatch.NewFileSidecar(deps.Config.Paths.State + "/health-watch.json")
		return healthwatch.NewChecker(deps)
	}
	t.Cleanup(func() { initHealthWatchChecker = originalNewChecker })

	code := application.Run(context.Background(), []string{"host", "health-watch", "check", "--json", "--no-alert"})
	if code != ExitDegraded {
		t.Fatalf("exit code = %d, want %d\nstdout=%s\nstderr=%s", code, ExitDegraded, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), `"healthy": false`) || !strings.Contains(out.String(), "stale-heartbeat") {
		t.Fatalf("stdout = %s", out.String())
	}
}
