package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/jobindex"
)

func TestAuditClassifiesReferencedPresentMissingAndUnreferencedFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	sink := newArtifactSinkForTest(t, root, store, defaultArtifactPolicy())
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)

	presentLog := filepath.Join(root, "logs", "present.log")
	missingLog := filepath.Join(root, "logs", "missing.log")
	oldOrphan := filepath.Join(root, "logs", "old-orphan.log")
	recentOrphan := filepath.Join(root, "logs", "recent-orphan.log")
	inFlight := filepath.Join(root, "logs", ".ci-runner-log-live.tmp")
	for _, spec := range []struct {
		path string
		when time.Time
	}{
		{path: presentLog, when: recent},
		{path: oldOrphan, when: old},
		{path: recentOrphan, when: now.Add(-30 * time.Minute)},
		{path: inFlight, when: now.Add(-30 * time.Minute)},
	} {
		if err := os.WriteFile(spec.path, []byte("evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(spec.path, spec.when, spec.when); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Upsert(context.Background(), jobindex.Patch{
		PoolID: "org", RunnerName: "present", ContainerID: "container-present",
		LogPath: presentLog, DiagnosticPath: "", ArtifactStartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(context.Background(), jobindex.Patch{
		PoolID: "org", RunnerName: "missing", ContainerID: "container-missing",
		LogPath: missingLog, DiagnosticPath: "", ArtifactStartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	report, err := sink.Audit(context.Background(), nil, false, "docker engine unavailable")
	if err != nil {
		t.Fatal(err)
	}
	if !report.CatalogAvailable || report.CatalogError != "" {
		t.Fatalf("catalog availability = %t error=%q", report.CatalogAvailable, report.CatalogError)
	}
	if report.InventoryAvailable || report.InventoryError != "docker engine unavailable" {
		t.Fatalf("inventory availability = %t error=%q", report.InventoryAvailable, report.InventoryError)
	}

	logDirectory := findAuditDirectory(t, report, filepath.Join(root, "logs"))
	assertAuditFile(t, logDirectory, presentLog, ArtifactReferencedPresent, false)
	assertAuditFile(t, logDirectory, oldOrphan, ArtifactUnreferencedPastCutoff, false)
	assertAuditFile(t, logDirectory, recentOrphan, ArtifactUnreferencedWithinCutoff, false)
	assertAuditFile(t, logDirectory, inFlight, ArtifactUnreferencedWithinCutoff, true)

	if len(report.ReferencedMissing) != 1 || report.ReferencedMissing[0].Path != missingLog {
		t.Fatalf("referenced missing = %#v, want %q", report.ReferencedMissing, missingLog)
	}
}

func TestAuditWorksWhenTheCatalogIsAbsent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	sink := newArtifactSinkForTest(t, root, store, defaultArtifactPolicy())
	orphan := filepath.Join(root, "logs", "catalog-absent.log")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	report, err := sink.Audit(context.Background(), nil, false, "")
	if err != nil {
		t.Fatal(err)
	}
	logDirectory := findAuditDirectory(t, report, filepath.Join(root, "logs"))
	assertAuditFile(t, logDirectory, orphan, ArtifactUnreferencedPastCutoff, false)
}

func TestAuditReportsTombstonedAdoptedContainersWhenInventoryIsAvailable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	sink := newArtifactSinkForTest(t, root, store, defaultArtifactPolicy())
	tombstone := time.Now().UTC()
	if _, err := store.Upsert(context.Background(), jobindex.Patch{
		PoolID: "org", RunnerName: "frozen", ContainerID: "container-old",
		TombstonedAt: &tombstone,
	}); err != nil {
		t.Fatal(err)
	}
	adopted := []ArtifactMetadata{{
		ContainerID: "container-live", PoolID: "org", WorkerName: "frozen",
		StartedAt: time.Unix(1, 0).UTC(),
	}}

	report, err := sink.Audit(context.Background(), adopted, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.TombstonedAdopted) != 1 {
		t.Fatalf("tombstoned adopted = %#v", report.TombstonedAdopted)
	}
	if report.TombstonedAdopted[0].ContainerID != "container-live" {
		t.Fatalf("tombstoned adopted container = %#v", report.TombstonedAdopted[0])
	}
}

func TestPurgeDryRunSkipsInFlightTemporaryFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	sink := newArtifactSinkForTest(t, root, store, defaultArtifactPolicy())
	old := time.Now().Add(-48 * time.Hour)
	orphan := filepath.Join(root, "logs", "purge-me.log")
	inFlight := filepath.Join(root, "logs", ".ci-runner-diag-live.tmp")
	for _, spec := range []struct {
		path string
	}{
		{path: orphan},
		{path: inFlight},
	} {
		if err := os.WriteFile(spec.path, []byte("evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(spec.path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	result, err := sink.PurgeUnreferenced(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeleteCount != 1 || len(result.Deleted) != 1 || result.Deleted[0].Path != orphan {
		t.Fatalf("purge dry-run deleted = %#v", result.Deleted)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Path != inFlight {
		t.Fatalf("purge dry-run skipped = %#v", result.Skipped)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("dry-run removed orphan: %v", err)
	}
}

func TestPurgeConfirmRemovesUnreferencedNonTemporaryFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	sink := newArtifactSinkForTest(t, root, store, defaultArtifactPolicy())
	orphan := filepath.Join(root, "logs", "purge-me.log")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.WriteFile(orphan, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	result, err := sink.PurgeUnreferenced(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeleteCount != 1 {
		t.Fatalf("purge delete count = %d", result.DeleteCount)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan still exists: %v", err)
	}
}

func findAuditDirectory(t *testing.T, report ArtifactAuditReport, want string) ArtifactAuditDirectory {
	t.Helper()
	for _, directory := range report.Directories {
		if directory.Directory == want {
			return directory
		}
	}
	t.Fatalf("audit directories = %#v, want %q", report.Directories, want)
	return ArtifactAuditDirectory{}
}

func assertAuditFile(t *testing.T, directory ArtifactAuditDirectory, path string, class ArtifactFileClass, inFlight bool) {
	t.Helper()
	for _, file := range directory.Files {
		if file.Path != path {
			continue
		}
		if file.Classification != class || file.InFlightTemporary != inFlight {
			t.Fatalf("file %q classification = %q inFlight=%t, want %q inFlight=%t", path, file.Classification, file.InFlightTemporary, class, inFlight)
		}
		return
	}
	t.Fatalf("file %q not found in %#v", path, directory.Files)
}
