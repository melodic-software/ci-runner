package jobindex

import (
	"context"
	"errors"
	"testing"
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
