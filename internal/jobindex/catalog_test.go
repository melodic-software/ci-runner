package jobindex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSchemaVersionOneEncodingRemainsStrictlyReadableByV019(t *testing.T) {
	t.Parallel()
	type legacyRecord struct {
		PoolID            string     `json:"poolId"`
		RunnerName        string     `json:"runnerName"`
		ContainerID       string     `json:"containerId,omitempty"`
		JobID             string     `json:"jobId,omitempty"`
		Result            string     `json:"result,omitempty"`
		LogPath           string     `json:"logPath,omitempty"`
		DiagnosticPath    string     `json:"diagnosticPath,omitempty"`
		ArtifactStartedAt time.Time  `json:"artifactStartedAt,omitempty"`
		JobStartedAt      time.Time  `json:"jobStartedAt,omitempty"`
		CompletedAt       time.Time  `json:"completedAt,omitempty"`
		FinalizedAt       time.Time  `json:"finalizedAt,omitempty"`
		UpdatedAt         time.Time  `json:"updatedAt"`
		Open              bool       `json:"open"`
		TombstonedAt      *time.Time `json:"tombstonedAt,omitempty"`
	}
	type legacyCatalog struct {
		SchemaVersion int            `json:"schemaVersion"`
		Records       []legacyRecord `json:"records"`
	}

	now := time.Unix(500, 0).UTC()
	tombstonedAt := now.Add(time.Minute)
	encoded, err := json.Marshal(Catalog{SchemaVersion: SchemaVersion, Records: []Record{{
		PoolID: "org", RunnerName: "runner", ContainerID: "container", JobID: "job", Result: "Succeeded",
		LogPath: filepath.Join(t.TempDir(), "worker.log"), DiagnosticPath: filepath.Join(t.TempDir(), "worker-diag.tar.gz"),
		ArtifactStartedAt: now, JobStartedAt: now, CompletedAt: now, FinalizedAt: now,
		UpdatedAt: now, Open: false, TombstonedAt: &tombstonedAt,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"resourcePath"`)) {
		t.Fatalf("schemaVersion 1 unexpectedly persisted resourcePath: %s", encoded)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var decoded legacyCatalog
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("v0.1.9 strict decoder rejected current schemaVersion 1 jobs.json: %v\n%s", err, encoded)
	}
	if decoded.SchemaVersion != SchemaVersion || len(decoded.Records) != 1 || decoded.Records[0].RunnerName != "runner" {
		t.Fatalf("legacy decode = %#v", decoded)
	}
}

func TestResourceEvidencePathDerivesFromLegacyDiagnosticPath(t *testing.T) {
	t.Parallel()
	diagnosticPath := filepath.Join(t.TempDir(), "worker-diag.tar.gz")
	path, err := ResourceEvidencePath(Record{DiagnosticPath: diagnosticPath})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(diagnosticPath, "-diag.tar.gz") + "-resources.json"; path != want {
		t.Fatalf("resource evidence path = %q, want %q", path, want)
	}
	if _, err := ResourceEvidencePath(Record{DiagnosticPath: filepath.Join(t.TempDir(), "unexpected.tar.gz")}); err == nil {
		t.Fatal("unexpected diagnostic path derived a resource sidecar")
	}
}

func TestEventThenArtifactAndArtifactThenEventConverge(t *testing.T) {
	t.Parallel()
	for _, eventFirst := range []bool{true, false} {
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

func TestSaveCompactsOldestTombstonesUnderCapacityPressure(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store := newFileStoreForTest(t, directory)
	now := time.Unix(500, 0).UTC()
	store.now = func() time.Time { return now }
	// A synthetic catalog just over the save cap: mostly old tombstones plus
	// one live record and one fresh tombstone that must both survive.
	catalog := Catalog{SchemaVersion: SchemaVersion}
	padding := strings.Repeat("x", 512)
	recordCount := maximumJobState/len(padding) + 64
	for index := range recordCount {
		tombstone := now.Add(-time.Duration(recordCount-index) * time.Minute)
		catalog.Records = append(catalog.Records, Record{
			PoolID: "org", RunnerName: fmt.Sprintf("runner-%07d", index),
			JobID:     fmt.Sprintf("job-%07d-%s", index, padding),
			UpdatedAt: now, TombstonedAt: &tombstone,
		})
	}
	live := Record{PoolID: "org", RunnerName: "runner-live", JobID: "job-live", JobStartedAt: now, UpdatedAt: now}
	catalog.Records = append(catalog.Records, live)
	Sort(&catalog)
	if encoded, err := json.MarshalIndent(catalog, "", "  "); err != nil {
		t.Fatal(err)
	} else if len(encoded) <= maximumJobState {
		t.Fatalf("fixture must exceed the save cap, got %d bytes", len(encoded))
	}
	if err := store.saveUnlocked(catalog); err != nil {
		t.Fatalf("save under capacity pressure = %v", err)
	}
	info, err := os.Stat(filepath.Join(directory, jobsFilename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maximumJobState {
		t.Fatalf("saved jobs.json is %d bytes, above the %d-byte cap", info.Size(), maximumJobState)
	}
	reloaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	survivors := make(map[string]Record, len(reloaded.Records))
	for _, record := range reloaded.Records {
		survivors[record.RunnerName] = record
	}
	if _, ok := survivors["runner-live"]; !ok {
		t.Fatal("live record was compacted away")
	}
	newestTombstoneName := fmt.Sprintf("runner-%07d", recordCount-1)
	if _, ok := survivors[newestTombstoneName]; !ok {
		t.Fatal("newest tombstone was compacted before older ones")
	}
	oldestTombstoneName := "runner-0000000"
	if _, ok := survivors[oldestTombstoneName]; ok {
		t.Fatal("oldest tombstone survived capacity compaction")
	}
	if len(reloaded.Records) >= recordCount+1 {
		t.Fatalf("no records were compacted: %d", len(reloaded.Records))
	}
}

func TestSaveCompactsOldestCompletedRecordsWhenNoTombstonesRemain(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store := newFileStoreForTest(t, directory)
	now := time.Unix(550, 0).UTC()
	store.now = func() time.Time { return now }
	// The #98 incident shape: a catalog over the save cap made entirely of
	// completed, never-tombstoned records, plus an open record and an active
	// (started, not completed) record that must both survive compaction.
	catalog := Catalog{SchemaVersion: SchemaVersion}
	padding := strings.Repeat("w", 512)
	recordCount := maximumJobState/len(padding) + 64
	for index := range recordCount {
		completed := now.Add(-time.Duration(recordCount-index) * time.Minute)
		catalog.Records = append(catalog.Records, Record{
			PoolID: "org", RunnerName: fmt.Sprintf("runner-%07d", index),
			JobID:  fmt.Sprintf("job-%07d-%s", index, padding),
			Result: "succeeded", JobStartedAt: completed.Add(-time.Minute),
			CompletedAt: completed, FinalizedAt: completed, UpdatedAt: now,
		})
	}
	open := Record{PoolID: "org", RunnerName: "runner-open", ContainerID: "container-open", Open: true, UpdatedAt: now}
	active := Record{PoolID: "org", RunnerName: "runner-active", JobID: "job-active", JobStartedAt: now, UpdatedAt: now}
	// Finalized by stale-open reconciliation while its completion event is
	// still in flight — ActiveJob treats this as active, so it must survive.
	finalizedActive := Record{
		PoolID: "org", RunnerName: "runner-finalized-active", JobID: "job-finalized-active",
		JobStartedAt: now.Add(-2 * time.Minute), FinalizedAt: now.Add(-time.Minute), UpdatedAt: now,
	}
	catalog.Records = append(catalog.Records, open, active, finalizedActive)
	Sort(&catalog)
	if encoded, err := json.MarshalIndent(catalog, "", "  "); err != nil {
		t.Fatal(err)
	} else if len(encoded) <= maximumJobState {
		t.Fatalf("fixture must exceed the save cap, got %d bytes", len(encoded))
	}
	if err := store.saveUnlocked(catalog); err != nil {
		t.Fatalf("save under capacity pressure = %v", err)
	}
	info, err := os.Stat(filepath.Join(directory, jobsFilename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maximumJobState {
		t.Fatalf("saved jobs.json is %d bytes, above the %d-byte cap", info.Size(), maximumJobState)
	}
	reloaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	survivors := make(map[string]Record, len(reloaded.Records))
	for _, record := range reloaded.Records {
		survivors[record.RunnerName] = record
	}
	if _, ok := survivors["runner-open"]; !ok {
		t.Fatal("open record was compacted away")
	}
	if _, ok := survivors["runner-active"]; !ok {
		t.Fatal("active record was compacted away")
	}
	if _, ok := survivors["runner-finalized-active"]; !ok {
		t.Fatal("finalized-but-still-active record was compacted away")
	}
	newestCompletedName := fmt.Sprintf("runner-%07d", recordCount-1)
	if _, ok := survivors[newestCompletedName]; !ok {
		t.Fatal("newest completed record was compacted before older ones")
	}
	if _, ok := survivors["runner-0000000"]; ok {
		t.Fatal("oldest completed record survived capacity compaction")
	}
	if len(reloaded.Records) >= recordCount+3 {
		t.Fatalf("no records were compacted: %d", len(reloaded.Records))
	}
}

func TestSaveFailsClosedWhenLiveRecordsAloneExceedCapacity(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store := newFileStoreForTest(t, directory)
	now := time.Unix(600, 0).UTC()
	catalog := Catalog{SchemaVersion: SchemaVersion}
	padding := strings.Repeat("y", 512)
	recordCount := maximumJobState/len(padding) + 64
	for index := range recordCount {
		catalog.Records = append(catalog.Records, Record{
			PoolID: "org", RunnerName: fmt.Sprintf("runner-%07d", index),
			JobID:        fmt.Sprintf("job-%07d-%s", index, padding),
			JobStartedAt: now, UpdatedAt: now,
		})
	}
	Sort(&catalog)
	err := store.saveUnlocked(catalog)
	if err == nil || !strings.Contains(err.Error(), "safety limit") {
		t.Fatalf("expected safety-limit failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(directory, jobsFilename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed save must not leave jobs.json behind: %v", statErr)
	}
}

func TestLoadToleratesFilesAboveTheSaveCapAndNextSaveCompacts(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store := newFileStoreForTest(t, directory)
	now := time.Unix(700, 0).UTC()
	store.now = func() time.Time { return now }
	catalog := Catalog{SchemaVersion: SchemaVersion}
	padding := strings.Repeat("z", 512)
	recordCount := maximumJobState/len(padding) + 512
	for index := range recordCount {
		tombstone := now.Add(-time.Duration(recordCount-index) * time.Minute)
		catalog.Records = append(catalog.Records, Record{
			PoolID: "org", RunnerName: fmt.Sprintf("runner-%07d", index),
			JobID:     fmt.Sprintf("job-%07d-%s", index, padding),
			UpdatedAt: now, TombstonedAt: &tombstone,
		})
	}
	Sort(&catalog)
	encoded, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) <= maximumJobState || len(encoded) > maximumJobStateLoad {
		t.Fatalf("fixture must sit between the save cap and load limit, got %d bytes", len(encoded))
	}
	if err := os.WriteFile(filepath.Join(directory, jobsFilename), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatalf("load of a cap-breached file = %v", err)
	}
	if _, err := store.Upsert(context.Background(), Patch{PoolID: "org", RunnerName: "runner-recovery", JobID: "job-recovery"}); err != nil {
		t.Fatalf("upsert against a cap-breached file = %v", err)
	}
	info, err := os.Stat(filepath.Join(directory, jobsFilename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maximumJobState {
		t.Fatalf("recovered jobs.json is %d bytes, above the %d-byte cap", info.Size(), maximumJobState)
	}
}

func newFileStoreWithDependencies(t *testing.T, directory string, locker *testLocker) *FileStore {
	t.Helper()
	store, err := NewFileStore(directory, locker, testACL{})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
