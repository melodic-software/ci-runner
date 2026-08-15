package jobindex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	statefs "github.com/melodic-software/ci-runner/internal/state/fs"
)

func TestSnapshotBytesReturnsCommittedDocument(t *testing.T) {
	t.Parallel()
	store := newFileStoreForTest(t, t.TempDir())
	if _, err := store.SnapshotBytes(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound before any save", err)
	}
	if _, err := store.Upsert(context.Background(), Patch{PoolID: "org", RunnerName: "runner-a"}); err != nil {
		t.Fatal(err)
	}
	contents, err := store.SnapshotBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := DecodeCatalog(contents)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Records) != 1 || catalog.Records[0].RunnerName != "runner-a" {
		t.Fatalf("catalog = %#v, want the committed record", catalog)
	}
}

// TestSnapshotBytesBoundsOversizedFileOnDisk pins the disk-read bound with a
// real oversized file: the read stops at the load tolerance instead of
// filling memory, and both decode paths then reject the document.
func TestSnapshotBytesBoundsOversizedFileOnDisk(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store := newFileStoreForTest(t, directory)
	if err := os.WriteFile(filepath.Join(directory, jobsFilename), make([]byte, maximumJobStateLoad+2), 0o600); err != nil {
		t.Fatal(err)
	}
	contents, err := store.SnapshotBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != maximumJobStateLoad+1 {
		t.Fatalf("read %d bytes, want the read bounded at %d", len(contents), maximumJobStateLoad+1)
	}
	if _, err := DecodeCatalog(contents); err == nil {
		t.Fatal("expected the load safety limit to reject the snapshot")
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("expected the load safety limit to reject the store load")
	}
}

// TestSnapshotBytesSerializesWithConcurrentSaves exercises snapshot reads
// against concurrent saves through the real platform locker. Without the
// store lock, a Windows save's MoveFileEx replace fails with a sharing
// violation whenever a reader holds the file open, so any save error here is
// a regression to the out-of-lock read.
func TestSnapshotBytesSerializesWithConcurrentSaves(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	locker, err := statefs.NewPlatformLocker(directory)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(directory, locker, testACL{})
	if err != nil {
		t.Fatal(err)
	}
	const iterations = 100
	var group sync.WaitGroup
	failures := make(chan error, 2*iterations)
	group.Add(2)
	go func() {
		defer group.Done()
		for i := 0; i < iterations; i++ {
			if _, err := store.Upsert(context.Background(), Patch{PoolID: "org", RunnerName: fmt.Sprintf("runner-%03d", i)}); err != nil {
				failures <- fmt.Errorf("upsert %d: %w", i, err)
			}
		}
	}()
	go func() {
		defer group.Done()
		for i := 0; i < iterations; i++ {
			if _, err := store.SnapshotBytes(context.Background()); err != nil && !errors.Is(err, ErrNotFound) {
				failures <- fmt.Errorf("snapshot %d: %w", i, err)
			}
		}
	}()
	group.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}
