package jobindex

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEventThenArtifactAndArtifactThenEventConverge(t *testing.T) {
	t.Parallel()
	for _, eventFirst := range []bool{true, false} {
		eventFirst := eventFirst
		t.Run(map[bool]string{true: "event-first", false: "artifact-first"}[eventFirst], func(t *testing.T) {
			t.Parallel()
			store := newFileStoreForTest(t, t.TempDir())
			now := time.Unix(100, 0).UTC()
			events := EventSink{Store: store, Now: func() time.Time { return now }}
			artifact := Patch{
				PoolID: "org", RunnerName: "runner-1", ContainerID: "container-1",
				LogPath: filepath.Join(t.TempDir(), "worker.log"), ArtifactStartedAt: now.Add(-time.Minute),
			}
			writeEvent := func() {
				t.Helper()
				if err := events.JobStarted(context.Background(), "org", "runner-1", "42"); err != nil {
					t.Fatal(err)
				}
			}
			writeArtifact := func() {
				t.Helper()
				if _, err := store.Upsert(context.Background(), artifact); err != nil {
					t.Fatal(err)
				}
			}
			if eventFirst {
				writeEvent()
				writeArtifact()
			} else {
				writeArtifact()
				writeEvent()
			}
			record, err := store.FindByJobID(context.Background(), "42")
			if err != nil {
				t.Fatal(err)
			}
			if record.PoolID != "org" || record.RunnerName != "runner-1" || record.ContainerID != "container-1" || record.LogPath != artifact.LogPath {
				t.Fatalf("merged record = %#v", record)
			}
		})
	}
}

func TestFileStorePersistsExactLookupAndTombstoneAcrossRestart(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	locker := &testLocker{}
	store := newFileStoreWithDependencies(t, directory, locker)
	now := time.Unix(200, 0).UTC()
	store.now = func() time.Time { return now }
	for _, patch := range []Patch{
		{PoolID: "org", RunnerName: "runner-a", JobID: "12"},
		{PoolID: "org", RunnerName: "runner-b", JobID: "123"},
	} {
		if _, err := store.Upsert(context.Background(), patch); err != nil {
			t.Fatal(err)
		}
	}

	reopened := newFileStoreWithDependencies(t, directory, locker)
	record, err := reopened.FindByJobID(context.Background(), "12")
	if err != nil {
		t.Fatal(err)
	}
	if record.RunnerName != "runner-a" {
		t.Fatalf("exact lookup returned %#v", record)
	}
	tombstone := now.Add(time.Hour)
	if _, err := reopened.Upsert(context.Background(), Patch{
		PoolID: "org", RunnerName: "runner-a", JobID: "12", TombstonedAt: &tombstone,
	}); err != nil {
		t.Fatal(err)
	}
	third := newFileStoreWithDependencies(t, directory, locker)
	if _, err := third.FindByJobID(context.Background(), "12"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tombstoned exact lookup error = %v", err)
	}
	if _, err := third.FindByJobID(context.Background(), "123"); err != nil {
		t.Fatalf("neighboring job was lost: %v", err)
	}
}

func TestImmutableIdentityConflictsAndIdempotentRedelivery(t *testing.T) {
	t.Parallel()
	store := newFileStoreForTest(t, t.TempDir())
	events := EventSink{Store: store, Now: func() time.Time { return time.Unix(300, 0).UTC() }}
	for range 2 {
		if err := events.JobStarted(context.Background(), "org", "runner", "job"); err != nil {
			t.Fatal(err)
		}
	}
	if err := events.JobStarted(context.Background(), "org", "runner", "different-job"); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting redelivery error = %v", err)
	}
}

func TestActiveJobSurvivesRestartAndCompletionClearsIt(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	locker := &testLocker{}
	store := newFileStoreWithDependencies(t, directory, locker)
	events := EventSink{Store: store, Now: func() time.Time { return time.Unix(350, 0).UTC() }}
	if err := events.JobStarted(context.Background(), "org", "runner", "job-1"); err != nil {
		t.Fatal(err)
	}
	reopened := newFileStoreWithDependencies(t, directory, locker)
	if jobID, active, err := reopened.ActiveJob(context.Background(), "org", "runner"); err != nil || !active || jobID != "job-1" {
		t.Fatalf("reopened active job = %q %t, err=%v", jobID, active, err)
	}
	if err := events.JobCompleted(context.Background(), "org", "runner", "job-1", "Succeeded"); err != nil {
		t.Fatal(err)
	}
	if _, active, err := reopened.ActiveJob(context.Background(), "org", "runner"); err != nil || active {
		t.Fatalf("completed active = %t, err=%v", active, err)
	}
}

func TestActiveJobFailsClosedOnTombstoneConflict(t *testing.T) {
	t.Parallel()
	store := newFileStoreForTest(t, t.TempDir())
	now := time.Unix(360, 0).UTC()
	store.now = func() time.Time { return now }
	if _, err := store.Upsert(context.Background(), Patch{PoolID: "org", RunnerName: "runner", JobID: "job-1", JobStartedAt: now}); err != nil {
		t.Fatal(err)
	}
	tombstone := now.Add(time.Minute)
	if _, err := store.Upsert(context.Background(), Patch{PoolID: "org", RunnerName: "runner", TombstonedAt: &tombstone}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ActiveJob(context.Background(), "org", "runner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("active tombstone conflict = %v", err)
	}
}

func TestTombstonesAreAtomicallyCompactedAfterRetention(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store := newFileStoreForTest(t, directory)
	now := time.Unix(400, 0).UTC()
	for index, age := range []time.Duration{2 * time.Hour, 30 * time.Minute} {
		runner := fmt.Sprintf("runner-%d", index)
		if _, err := store.Upsert(context.Background(), Patch{PoolID: "org", RunnerName: runner, JobID: runner}); err != nil {
			t.Fatal(err)
		}
		tombstone := now.Add(-age)
		if _, err := store.Upsert(context.Background(), Patch{PoolID: "org", RunnerName: runner, JobID: runner, TombstonedAt: &tombstone}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := store.PruneTombstones(context.Background(), now.Add(-time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d error=%v", removed, err)
	}
	reopened := newFileStoreForTest(t, directory)
	catalog, err := reopened.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Records) != 1 || catalog.Records[0].RunnerName != "runner-1" {
		t.Fatalf("compacted catalog = %#v", catalog)
	}
}

func TestFileStoreHardensAndVerifiesDirectoryTemporaryAndFinalState(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	acl := &recordingIndexACL{}
	store, err := NewFileStore(directory, &testLocker{}, acl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(context.Background(), Patch{PoolID: "org", RunnerName: "runner", JobID: "job"}); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(directory, jobsFilename)
	if !acl.sawBoth(directory) || !acl.sawBoth(finalPath) || !acl.sawTemporary() {
		t.Fatalf("ACL calls hardened=%v verified=%v", acl.hardened, acl.verified)
	}
}

type testLocker struct{ mu sync.Mutex }

func (l *testLocker) Lock(context.Context) (func() error, error) {
	l.mu.Lock()
	return func() error { l.mu.Unlock(); return nil }, nil
}

type testACL struct{}

func (testACL) Harden(string) error { return nil }
func (testACL) Verify(string) error { return nil }

type recordingIndexACL struct {
	mu       sync.Mutex
	hardened []string
	verified []string
}

func (a *recordingIndexACL) Harden(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hardened = append(a.hardened, filepath.Clean(path))
	return nil
}
func (a *recordingIndexACL) Verify(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.verified = append(a.verified, filepath.Clean(path))
	return nil
}
func (a *recordingIndexACL) sawBoth(path string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	path = filepath.Clean(path)
	return containsPath(a.hardened, path) && containsPath(a.verified, path)
}
func (a *recordingIndexACL) sawTemporary() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, path := range a.hardened {
		if strings.HasPrefix(filepath.Base(path), ".jobs.json-") && containsPath(a.verified, path) {
			return true
		}
	}
	return false
}

func containsPath(paths []string, expected string) bool {
	for _, path := range paths {
		if path == expected {
			return true
		}
	}
	return false
}

func newFileStoreForTest(t *testing.T, directory string) *FileStore {
	t.Helper()
	return newFileStoreWithDependencies(t, directory, &testLocker{})
}

func newFileStoreWithDependencies(t *testing.T, directory string, locker *testLocker) *FileStore {
	t.Helper()
	store, err := NewFileStore(directory, locker, testACL{})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
