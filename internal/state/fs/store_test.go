package statefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

type recordingACL struct {
	mu    sync.Mutex
	paths []string
}

type unlockErrorLocker struct{ err error }

func (l unlockErrorLocker) Lock(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() error { return l.err }, nil
}

func (a *recordingACL) Harden(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.paths = append(a.paths, path)
	return nil
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "state")
	locker, err := NewPlatformLocker(directory)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(directory, locker, &recordingACL{})
	if err != nil {
		t.Fatal(err)
	}
	return store, directory
}

func TestStoreRoundTripsDesiredAndObserved(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 9, 1, 2, 3, 0, time.UTC)
	capacity := 4
	desired := model.DesiredState{SchemaVersion: 1, Mode: model.ModeGaming, TemporaryCapacityOverride: &capacity, UpdatedAt: now}
	if err := store.SaveDesired(ctx, desired); err != nil {
		t.Fatalf("SaveDesired: %v", err)
	}
	gotDesired, err := store.LoadDesired(ctx)
	if err != nil {
		t.Fatalf("LoadDesired: %v", err)
	}
	if gotDesired.Mode != desired.Mode || *gotDesired.TemporaryCapacityOverride != capacity {
		t.Fatalf("desired mismatch: %#v", gotDesired)
	}
	observed := model.ObservedState{SchemaVersion: 1, Phase: model.PhaseGaming, HeartbeatAt: now, Version: "1.2.3"}
	if err := store.SaveObserved(ctx, observed); err != nil {
		t.Fatalf("SaveObserved: %v", err)
	}
	gotObserved, err := store.LoadObserved(ctx)
	if err != nil {
		t.Fatalf("LoadObserved: %v", err)
	}
	if gotObserved.Phase != model.PhaseGaming || gotObserved.Version != "1.2.3" {
		t.Fatalf("observed mismatch: %#v", gotObserved)
	}
	receipt := model.RestartReceipt{
		SchemaVersion: 1, RequestID: "restart-request-1", ProcessID: 1234,
		Version: "1.2.3", CompletedAt: now,
	}
	if err := store.SaveRestartReceipt(ctx, receipt); err != nil {
		t.Fatalf("SaveRestartReceipt: %v", err)
	}
	gotReceipt, err := store.LoadRestartReceipt(ctx)
	if err != nil {
		t.Fatalf("LoadRestartReceipt: %v", err)
	}
	if gotReceipt != receipt {
		t.Fatalf("restart receipt mismatch: %#v", gotReceipt)
	}
}

// observed.json is read by operators during an incident, so quiesceReason has
// to be on the wire under that exact key when the controller is draining, and
// absent rather than empty or "none" when it is not.
func TestObservedQuiesceReasonIsOnTheWireOnlyWhileDraining(t *testing.T) {
	tests := []struct {
		name       string
		observed   model.ObservedState
		wantOnWire bool
	}{
		{
			name: "draining",
			observed: model.ObservedState{
				SchemaVersion: 1, Phase: model.PhaseDraining, HeartbeatAt: time.Now().UTC(),
				QuiesceReason: model.QuiesceReasonAdvertisementBudgetExhausted,
			},
			wantOnWire: true,
		},
		{
			name:     "ready",
			observed: model.ObservedState{SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: time.Now().UTC()},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, directory := newTestStore(t)
			ctx := context.Background()
			if err := store.SaveObserved(ctx, test.observed); err != nil {
				t.Fatalf("SaveObserved: %v", err)
			}

			contents, err := os.ReadFile(filepath.Join(directory, "observed.json"))
			if err != nil {
				t.Fatalf("read observed.json: %v", err)
			}
			if got := strings.Contains(string(contents), `"quiesceReason"`); got != test.wantOnWire {
				t.Fatalf("quiesceReason present = %v, want %v in %s", got, test.wantOnWire, contents)
			}

			// A round trip proves the persisted key matches the struct tag: a
			// mismatched tag would load an empty reason in the draining case.
			loaded, err := store.LoadObserved(ctx)
			if err != nil {
				t.Fatalf("LoadObserved: %v", err)
			}
			if loaded.QuiesceReason != test.observed.QuiesceReason {
				t.Fatalf("quiesce reason = %q, want %q", loaded.QuiesceReason, test.observed.QuiesceReason)
			}
		})
	}
}

// A controller reads observed.json files written by the release it replaced —
// and, after a rollback, by the release that replaced it, because the
// documented rollback order drains before restoring the prior pair. Unknown
// keys must therefore be ignored, top-level and nested, so an additive field
// in a newer release can never quarantine the file, force a capacity-zero
// recovery pass, or restart the drain clock on the older binary.
func TestObservedToleratesFieldsFromANewerRelease(t *testing.T) {
	store, directory := newTestStore(t)
	ctx := context.Background()
	drainStarted := time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC)
	if err := store.SaveObserved(ctx, model.ObservedState{
		SchemaVersion:  1,
		Phase:          model.PhaseDraining,
		HeartbeatAt:    drainStarted.Add(time.Minute),
		DrainStartedAt: &drainStarted,
		Pools:          []model.PoolObservation{{ID: "pool-a", UpdatedAt: drainStarted}},
	}); err != nil {
		t.Fatalf("SaveObserved: %v", err)
	}

	path := filepath.Join(directory, "observed.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read observed.json: %v", err)
	}
	withFutureKeys := string(contents)
	for _, injection := range []struct{ after, key string }{
		{`"schemaVersion": 1,`, `"aTopLevelKeyThisReaderDoesNotKnow": {"shape": ["compound"]},`},
		{`"id": "pool-a",`, `"aPoolKeyThisReaderDoesNotKnow": 7,`},
	} {
		next := strings.Replace(withFutureKeys, injection.after, injection.after+injection.key, 1)
		if next == withFutureKeys {
			t.Fatalf("could not inject %s; the observed.json layout changed", injection.key)
		}
		withFutureKeys = next
	}
	if err := os.WriteFile(path, []byte(withFutureKeys), 0o600); err != nil {
		t.Fatalf("write observed.json: %v", err)
	}

	loaded, err := store.LoadObserved(ctx)
	if err != nil {
		t.Fatalf("LoadObserved rejected a file carrying keys from a newer release: %v", err)
	}
	if loaded.Phase != model.PhaseDraining {
		t.Fatalf("phase = %q, want %q", loaded.Phase, model.PhaseDraining)
	}
	if loaded.DrainStartedAt == nil || !loaded.DrainStartedAt.Equal(drainStarted) {
		t.Fatalf("drainStartedAt = %v, want %v; the drain clock must survive unknown keys", loaded.DrainStartedAt, drainStarted)
	}
	if len(loaded.Pools) != 1 || loaded.Pools[0].ID != "pool-a" {
		t.Fatalf("pools = %#v, want the persisted pool-a entry", loaded.Pools)
	}
}

// Forward tolerance is scoped to unknown keys within schemaVersion 1. A
// schemaVersion this release does not support stays rejected — bumping it is
// the deliberate signal that an older release must not trust the file — and
// the single-JSON-value trailer check still catches concatenated writes.
func TestObservedStillRejectsIncompatibleFiles(t *testing.T) {
	tests := []struct {
		name      string
		corrupt   func(contents string) string
		wantInErr string
	}{
		{
			name: "schema version bump",
			corrupt: func(contents string) string {
				return strings.Replace(contents, `"schemaVersion": 1,`, `"schemaVersion": 2,`, 1)
			},
			wantInErr: "unsupported schemaVersion 2",
		},
		{
			name:      "trailing JSON value",
			corrupt:   func(contents string) string { return contents + "{}\n" },
			wantInErr: "multiple JSON values",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, directory := newTestStore(t)
			ctx := context.Background()
			if err := store.SaveObserved(ctx, model.ObservedState{
				SchemaVersion: 1, Phase: model.PhaseReady, HeartbeatAt: time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC),
			}); err != nil {
				t.Fatalf("SaveObserved: %v", err)
			}
			path := filepath.Join(directory, "observed.json")
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read observed.json: %v", err)
			}
			corrupted := test.corrupt(string(contents))
			if corrupted == string(contents) {
				t.Fatal("corruption had no effect; the observed.json layout changed")
			}
			if err := os.WriteFile(path, []byte(corrupted), 0o600); err != nil {
				t.Fatalf("write observed.json: %v", err)
			}
			_, err = store.LoadObserved(ctx)
			if err == nil {
				t.Fatal("LoadObserved accepted an incompatible file")
			}
			if !strings.Contains(err.Error(), test.wantInErr) {
				t.Fatalf("LoadObserved error = %v, want it to contain %q", err, test.wantInErr)
			}
		})
	}
}

func TestStoreRejectsInvalidRestartReceipts(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	valid := model.RestartReceipt{
		SchemaVersion: 1, RequestID: "request-1", ProcessID: 42,
		Version: "1.2.3", CompletedAt: time.Now().UTC(),
	}
	tests := map[string]func(*model.RestartReceipt){
		"schema":     func(value *model.RestartReceipt) { value.SchemaVersion = 0 },
		"request ID": func(value *model.RestartReceipt) { value.RequestID = "bad request" },
		"process ID": func(value *model.RestartReceipt) { value.ProcessID = 0 },
		"version":    func(value *model.RestartReceipt) { value.Version = "" },
		"completion": func(value *model.RestartReceipt) { value.CompletedAt = time.Time{} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			value := valid
			mutate(&value)
			if err := store.SaveRestartReceipt(context.Background(), value); err == nil {
				t.Fatal("invalid restart receipt was accepted")
			}
		})
	}
}

func TestStoreRoundTripsUnregisteredWorkerState(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	now := time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC)
	observed := model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseDegraded,
		HeartbeatAt:   now,
		Workers: []model.Worker{{
			ID: "container-1", PoolID: "org", Name: "runner-1", RunnerID: 42,
			State: model.WorkerUnregistered, StartedAt: now.Add(-time.Minute),
		}},
	}
	if err := store.SaveObserved(context.Background(), observed); err != nil {
		t.Fatalf("SaveObserved unregistered worker: %v", err)
	}
	loaded, err := store.LoadObserved(context.Background())
	if err != nil {
		t.Fatalf("LoadObserved unregistered worker: %v", err)
	}
	if len(loaded.Workers) != 1 || loaded.Workers[0].State != model.WorkerUnregistered || loaded.Workers[0].RunnerID != 42 {
		t.Fatalf("unregistered worker did not round trip: %#v", loaded.Workers)
	}
}

func TestStoreRejectsUnknownFieldsAndMultipleValues(t *testing.T) {
	store, directory := newTestStore(t)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, desiredFilename)
	for name, value := range map[string]string{
		"unknown":  `{"schemaVersion":1,"mode":"enabled","updatedAt":"2026-07-09T01:02:03Z","surprise":true}`,
		"multiple": `{"schemaVersion":1,"mode":"enabled","updatedAt":"2026-07-09T01:02:03Z"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadDesired(context.Background()); err == nil {
				t.Fatal("expected strict JSON rejection")
			}
		})
	}
}

func TestStoreMapsMissingFileToStateNotFound(t *testing.T) {
	store, _ := newTestStore(t)
	_, err := store.LoadDesired(context.Background())
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("got %v, want state.ErrNotFound", err)
	}
}

func TestStoreNeverLeavesTemporaryStateFiles(t *testing.T) {
	store, directory := newTestStore(t)
	desired := model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: time.Now().UTC()}
	for index := 0; index < 5; index++ {
		if err := store.SaveDesired(context.Background(), desired); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".desired.json-") {
			t.Fatalf("temporary file was not removed: %s", entry.Name())
		}
	}
}

func TestStorePropagatesUnlockFailures(t *testing.T) {
	unlockErr := errors.New("native mutex release failed")
	directory := filepath.Join(t.TempDir(), "state")
	store, err := New(directory, unlockErrorLocker{err: unlockErr}, &recordingACL{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.LoadDesired(context.Background()); !errors.Is(err, state.ErrNotFound) || !errors.Is(err, unlockErr) {
		t.Fatalf("missing-state error = %v, want state.ErrNotFound joined with unlock failure", err)
	}
	desired := model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: time.Now().UTC()}
	if err := store.SaveDesired(context.Background(), desired); !errors.Is(err, unlockErr) {
		t.Fatalf("SaveDesired error = %v, want unlock failure", err)
	}
	if _, err := os.Stat(filepath.Join(directory, desiredFilename)); err != nil {
		t.Fatalf("state file was not committed before unlock failure: %v", err)
	}
}

func TestQuarantineObservedPreservesExactEvidenceUntilAtomicRecovery(t *testing.T) {
	store, directory := newTestStore(t)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{"schemaVersion":1,"phase":`)
	source := filepath.Join(directory, observedFilename)
	if err := os.WriteFile(source, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineObserved(context.Background()); err != nil {
		t.Fatal(err)
	}
	stillCorrupt, err := os.ReadFile(source)
	if err != nil || string(stillCorrupt) != string(corrupt) {
		t.Fatalf("source changed before recovery: bytes=%q err=%v", stillCorrupt, err)
	}
	matches, err := filepath.Glob(filepath.Join(directory, "observed.corrupt-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantine matches=%v err=%v", matches, err)
	}
	evidence, err := os.ReadFile(matches[0])
	if err != nil || string(evidence) != string(corrupt) {
		t.Fatalf("evidence=%q err=%v", evidence, err)
	}
	recovered := model.ObservedState{SchemaVersion: 1, Phase: model.PhaseDegraded, HeartbeatAt: time.Now().UTC()}
	if err := store.SaveObserved(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadObserved(context.Background()); err != nil {
		t.Fatal(err)
	}
	evidence, err = os.ReadFile(matches[0])
	if err != nil || string(evidence) != string(corrupt) {
		t.Fatalf("recovery lost quarantined evidence: %q err=%v", evidence, err)
	}
}
