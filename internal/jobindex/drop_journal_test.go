package jobindex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDropEntryArtifactPathsIncludesDerivedResourceSidecar(t *testing.T) {
	t.Parallel()
	diagnosticPath := filepath.Join(t.TempDir(), "worker-diag.tar.gz")
	entry := DropEntry{DiagnosticPath: diagnosticPath}
	paths := entry.ArtifactPaths()
	if len(paths) != 2 {
		t.Fatalf("artifact paths = %#v, want diagnostic and resource sidecar", paths)
	}
	wantSidecar := strings.TrimSuffix(diagnosticPath, "-diag.tar.gz") + "-resources.json"
	if paths[1] != wantSidecar {
		t.Fatalf("resource sidecar = %q, want %q", paths[1], wantSidecar)
	}
}

func TestSaveRecordsEveryTier2DropInTheDropJournal(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store := newFileStoreForTest(t, directory)
	now := time.Unix(800, 0).UTC()
	store.now = func() time.Time { return now }
	logDirectory := filepath.Join(directory, "logs")
	diagnosticDirectory := filepath.Join(directory, "diag")
	if err := os.MkdirAll(logDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(diagnosticDirectory, 0o700); err != nil {
		t.Fatal(err)
	}

	catalog := Catalog{SchemaVersion: SchemaVersion}
	// Keep the fixture comfortably under the drop-journal byte cap on Windows,
	// where temp-dir artifact paths are longer than on Linux.
	padding := strings.Repeat("v", 704)
	recordCount := maximumJobState/len(padding) + 64
	droppedRunners := make(map[string]Record, recordCount)
	for index := range recordCount {
		completed := now.Add(-time.Duration(recordCount-index) * time.Minute)
		runner := fmt.Sprintf("runner-%07d", index)
		record := Record{
			PoolID: "org", RunnerName: runner,
			JobID: fmt.Sprintf("job-%07d-%s", index, padding), ContainerID: fmt.Sprintf("container-%07d", index),
			Result: "succeeded", JobStartedAt: completed.Add(-time.Minute),
			LogPath:        filepath.Join(logDirectory, runner+".log"),
			DiagnosticPath: filepath.Join(diagnosticDirectory, runner+"-diag.tar.gz"),
			CompletedAt:    completed, FinalizedAt: completed, UpdatedAt: now,
		}
		catalog.Records = append(catalog.Records, record)
		droppedRunners[runner] = record
	}
	open := Record{PoolID: "org", RunnerName: "runner-open", ContainerID: "container-open", Open: true, UpdatedAt: now}
	active := Record{PoolID: "org", RunnerName: "runner-active", JobID: "job-active", JobStartedAt: now, UpdatedAt: now}
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
		t.Fatal(err)
	}

	journal, err := LoadDropJournal(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Entries) == 0 {
		t.Fatal("expected tier-2 drops in the journal")
	}

	reloaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	survivors := make(map[string]struct{}, len(reloaded.Records))
	for _, record := range reloaded.Records {
		survivors[record.RunnerName] = struct{}{}
	}
	expectedDrops := make(map[string]Record, len(droppedRunners))
	for runner, record := range droppedRunners {
		if _, ok := survivors[runner]; !ok {
			expectedDrops[runner] = record
		}
	}
	if len(expectedDrops) == 0 {
		t.Fatal("fixture did not drop any completed records")
	}

	journaledPaths := make(map[string]struct{})
	for _, entry := range journal.Entries {
		if entry.DroppedAt != now {
			t.Fatalf("entry droppedAt = %v, want %v", entry.DroppedAt, now)
		}
		if _, ok := survivors[entry.RunnerName]; ok {
			t.Fatalf("surviving record %q was also journaled as dropped", entry.RunnerName)
		}
		original, ok := expectedDrops[entry.RunnerName]
		if !ok {
			t.Fatalf("unexpected journaled runner %q", entry.RunnerName)
		}
		if entry.PoolID != original.PoolID || entry.JobID != original.JobID || entry.ContainerID != original.ContainerID ||
			entry.LogPath != original.LogPath || entry.DiagnosticPath != original.DiagnosticPath {
			t.Fatalf("journaled entry for %q has unexpected fields", entry.RunnerName)
		}
		for _, path := range entry.ArtifactPaths() {
			journaledPaths[path] = struct{}{}
		}
		delete(expectedDrops, entry.RunnerName)
	}
	if len(expectedDrops) != 0 {
		t.Fatalf("journal missed %d dropped runners", len(expectedDrops))
	}
	if len(journaledPaths) == 0 {
		t.Fatal("expected artifact paths in journaled drops")
	}

	// Phase 2.1's audit treats journal paths as unreferenced once the catalog
	// no longer maps them.
	referenced := make(map[string]struct{})
	for _, record := range reloaded.Records {
		if record.LogPath != "" {
			referenced[record.LogPath] = struct{}{}
		}
		if record.DiagnosticPath != "" {
			referenced[record.DiagnosticPath] = struct{}{}
		}
		if sidecar, err := ResourceEvidencePath(record); err == nil && sidecar != "" {
			referenced[sidecar] = struct{}{}
		}
	}
	for path := range journaledPaths {
		if _, ok := referenced[path]; ok {
			t.Fatalf("journaled artifact path %q is still referenced by the catalog", path)
		}
	}
}

func TestSaveDoesNotJournalTier1TombstoneDrops(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store := newFileStoreForTest(t, directory)
	now := time.Unix(810, 0).UTC()
	store.now = func() time.Time { return now }

	catalog := Catalog{SchemaVersion: SchemaVersion}
	padding := strings.Repeat("t", 512)
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
	if err := store.saveUnlocked(catalog); err != nil {
		t.Fatal(err)
	}
	journal, err := LoadDropJournal(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Entries) != 0 {
		t.Fatalf("tier-1 tombstone compaction must not write the drop journal, got %#v", journal.Entries)
	}
	if _, err := os.Stat(filepath.Join(directory, dropJournalFilename)); !os.IsNotExist(err) {
		t.Fatalf("unexpected drop journal file after tier-1-only compaction: %v", err)
	}
}

func TestDropJournalIsBoundedByMaximumJobState(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	now := time.Unix(820, 0).UTC()
	padding := strings.Repeat("p", 2048)
	var records []Record
	for index := range 4096 {
		completed := now.Add(-time.Duration(index) * time.Minute)
		runner := fmt.Sprintf("runner-%07d", index)
		records = append(records, Record{
			PoolID: "org", RunnerName: runner,
			JobID: fmt.Sprintf("job-%07d-%s", index, padding), Result: "succeeded",
			LogPath:        filepath.Join(directory, runner+".log"),
			DiagnosticPath: filepath.Join(directory, runner+"-diag.tar.gz"),
			JobStartedAt:   completed.Add(-time.Minute), CompletedAt: completed, FinalizedAt: completed, UpdatedAt: now,
		})
	}
	appendDropJournal(directory, testACL{}, records, now)
	info, err := os.Stat(filepath.Join(directory, dropJournalFilename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maximumJobState {
		t.Fatalf("drop journal is %d bytes, above the %d-byte cap", info.Size(), maximumJobState)
	}
	journal, err := LoadDropJournal(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Entries) == 0 || len(journal.Entries) >= len(records) {
		t.Fatalf("expected partial retention, got %d entries from %d drops", len(journal.Entries), len(records))
	}
	oldest := journal.Entries[0].RunnerName
	newest := journal.Entries[len(journal.Entries)-1].RunnerName
	if oldest >= newest {
		t.Fatalf("journal must drop oldest entries first, got oldest=%q newest=%q", oldest, newest)
	}
}
