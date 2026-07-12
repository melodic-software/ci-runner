package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/jobindex"
)

func TestWorkerStdoutTruncatesOnceWhileAcceptingTheEntireStream(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	limit := uint64(len(truncationMarker) + 8)
	sink := newArtifactSinkForTest(t, root, store, ArtifactPolicy{
		MaxFileSizeBytes: limit, RawDiagnosticMaxInputBytes: 1 << 20,
		Retention: time.Hour, TotalCapBytes: 1 << 20, CleanupEvery: time.Minute,
	})
	metadata := testArtifactMetadata("container-truncate", "runner-truncate")
	writer, err := sink.OpenLog(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{[]byte("1234567890"), bytes.Repeat([]byte("discarded"), 100)} {
		written, err := writer.Write(value)
		if err != nil || written != len(value) {
			t.Fatalf("Write returned %d/%d, %v", written, len(value), err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	record, err := store.FindByRunner(context.Background(), metadata.PoolID, metadata.WorkerName)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(record.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(content)) > limit || strings.Count(string(content), truncationMarker) != 1 {
		t.Fatalf("truncated log length=%d marker-count=%d content=%q", len(content), strings.Count(string(content), truncationMarker), content)
	}
}

func TestDiagnosticsRejectRawAndCompressedOverflowWithoutPublishingPartialFiles(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		maxFile    uint64
		maxInput   uint64
		inputBytes int
	}{
		{name: "raw-input", maxFile: 1 << 20, maxInput: 32, inputBytes: 33},
		{name: "compressed-output", maxFile: 64, maxInput: 4096, inputBytes: 2048},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			store := newTestJobStore(t, filepath.Join(root, "state"))
			sink := newArtifactSinkForTest(t, root, store, ArtifactPolicy{
				MaxFileSizeBytes: test.maxFile, RawDiagnosticMaxInputBytes: test.maxInput,
				Retention: time.Hour, TotalCapBytes: 1 << 20, CleanupEvery: time.Minute,
			})
			input := make([]byte, test.inputBytes)
			if _, err := rand.Read(input); err != nil {
				t.Fatal(err)
			}
			err := sink.WriteDiagnostics(context.Background(), testArtifactMetadata("container-overflow", "runner-overflow"), bytes.NewReader(input))
			if !errors.Is(err, ErrArtifactTooLarge) {
				t.Fatalf("overflow error = %v", err)
			}
			entries, readErr := os.ReadDir(filepath.Join(root, "diag"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("partial diagnostics were published: %v", entries)
			}
		})
	}
}

func TestFinalizeRequiresBothArtifactsWithoutClosingTheRecord(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	sink := newArtifactSinkForTest(t, root, store, defaultArtifactPolicy())
	metadata := testArtifactMetadata("container-incomplete", "runner-incomplete")
	writer, err := sink.OpenLog(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(context.Background(), metadata); err == nil {
		t.Fatal("incomplete artifacts finalized")
	}
	record, err := store.FindByRunner(context.Background(), metadata.PoolID, metadata.WorkerName)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Open || !record.FinalizedAt.IsZero() {
		t.Fatalf("incomplete record was made cleanup-eligible: %#v", record)
	}
}

func TestCleanupSkipsOpenAndAdoptedRecordsAndTombstonesExpiredMissingFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	sink := newArtifactSinkForTest(t, root, store, ArtifactPolicy{
		MaxFileSizeBytes: 1024, RawDiagnosticMaxInputBytes: 2048,
		Retention: time.Hour, TotalCapBytes: 1 << 20, CleanupEvery: time.Nanosecond,
	})
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	open := true
	closed := false
	logDirectory := filepath.Join(root, "logs")
	openPath := filepath.Join(logDirectory, "open.log")
	adoptedPath := filepath.Join(logDirectory, "adopted.log")
	for _, path := range []string{openPath, adoptedPath} {
		if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	patches := []jobindex.Patch{
		{PoolID: "org", RunnerName: "open", ContainerID: "container-open", LogPath: openPath, FinalizedAt: old, Open: &open},
		{PoolID: "org", RunnerName: "adopted", ContainerID: "container-adopted", LogPath: adoptedPath, FinalizedAt: old, Open: &closed},
		{PoolID: "org", RunnerName: "missing", ContainerID: "container-missing", LogPath: filepath.Join(logDirectory, "already-gone.log"), FinalizedAt: old, Open: &closed},
	}
	for _, patch := range patches {
		if _, err := store.Upsert(context.Background(), patch); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.AdoptAndCleanup(context.Background(), []ArtifactMetadata{
		testArtifactMetadata("container-open", "open"),
		testArtifactMetadata("container-adopted", "adopted"),
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{openPath, adoptedPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected artifact %q was removed: %v", path, err)
		}
	}
	if _, err := store.FindByRunner(context.Background(), "org", "missing"); !errors.Is(err, jobindex.ErrNotFound) {
		t.Fatalf("expired missing artifact was not tombstoned: %v", err)
	}
}

func TestTotalCapDeletesOldestFinalizedArtifactsFirst(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	sink := newArtifactSinkForTest(t, root, store, ArtifactPolicy{
		MaxFileSizeBytes: 10, RawDiagnosticMaxInputBytes: 20,
		Retention: 24 * time.Hour, TotalCapBytes: 15, CleanupEvery: time.Nanosecond,
	})
	closed := false
	now := time.Now().UTC()
	paths := map[string]string{}
	for index, name := range []string{"oldest", "newest"} {
		path := filepath.Join(root, "logs", name+".log")
		if err := os.WriteFile(path, bytes.Repeat([]byte{byte('a' + index)}, 10), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
		if _, err := store.Upsert(context.Background(), jobindex.Patch{
			PoolID: "org", RunnerName: name, ContainerID: "container-" + name, LogPath: path,
			FinalizedAt: now.Add(time.Duration(index-2) * time.Minute), Open: &closed,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.AdoptAndCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths["oldest"]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest artifact still exists: %v", err)
	}
	if _, err := os.Stat(paths["newest"]); err != nil {
		t.Fatalf("newest artifact was removed: %v", err)
	}
}

func TestCleanupRefusesIndexedPathsOutsideConfiguredRoots(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideRoot := t.TempDir()
	outside := filepath.Join(outsideRoot, "must-not-delete.log")
	if err := os.WriteFile(outside, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newTestJobStore(t, filepath.Join(root, "state"))
	closed := false
	if _, err := store.Upsert(context.Background(), jobindex.Patch{
		PoolID: "org", RunnerName: "escaped", ContainerID: "container-escaped",
		LogPath: outside, FinalizedAt: time.Now().Add(-2 * time.Hour), Open: &closed,
	}); err != nil {
		t.Fatal(err)
	}
	sink := newArtifactSinkForTest(t, root, store, defaultArtifactPolicy())
	if err := sink.CleanupNow(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "escapes configured root") {
		t.Fatalf("unsafe cleanup error = %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside evidence was touched: %v", err)
	}
}

func TestCleanupReconcilesStaleOpenRecordsAndRemovesOldOrphans(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	sink := newArtifactSinkForTest(t, root, store, defaultArtifactPolicy())
	open := true
	indexed := filepath.Join(root, "logs", "indexed.log")
	orphan := filepath.Join(root, "logs", ".abandoned.tmp")
	for _, path := range []string{indexed, orphan} {
		if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(context.Background(), jobindex.Patch{
		PoolID: "org", RunnerName: "stale", ContainerID: "container-stale", LogPath: indexed, Open: &open,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sink.CleanupNow(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	record, err := store.FindByRunner(context.Background(), "org", "stale")
	if err != nil {
		t.Fatal(err)
	}
	if record.Open || record.FinalizedAt.IsZero() {
		t.Fatalf("stale open record was not reconciled: %#v", record)
	}
	if _, err := os.Stat(indexed); err != nil {
		t.Fatalf("newly reconciled evidence was removed: %v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old orphan remains: %v", err)
	}
}

func TestDiagnosticsAtomicallyReplaceZeroAndOversizedRetryFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	policy := ArtifactPolicy{
		MaxFileSizeBytes: 128, RawDiagnosticMaxInputBytes: 1024,
		Retention: time.Hour, TotalCapBytes: 1024, CleanupEvery: time.Minute,
	}
	sink := newArtifactSinkForTest(t, root, store, policy)
	for index, initialSize := range []int{0, 129} {
		metadata := testArtifactMetadata("container-replace-"+string(rune('a'+index)), "runner-replace-"+string(rune('a'+index)))
		path := filepath.Join(root, "diag", artifactBaseName(metadata)+"-diag.tar.gz")
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), initialSize), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := sink.WriteDiagnostics(context.Background(), metadata, strings.NewReader("fresh diagnostics")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() <= 0 || info.Size() > int64(policy.MaxFileSizeBytes) {
			t.Fatalf("replacement size=%v error=%v", info, err)
		}
	}
}

func TestArtifactSinkHardensAndVerifiesEveryPublishedPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	acl := &recordingArtifactACL{}
	sink, err := NewFileArtifactSink(filepath.Join(root, "logs"), filepath.Join(root, "diag"), store, acl, defaultArtifactPolicy())
	if err != nil {
		t.Fatal(err)
	}
	metadata := testArtifactMetadata("container-acl", "runner-acl")
	writer, err := sink.OpenLog(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteDiagnostics(context.Background(), metadata, strings.NewReader("diagnostics")); err != nil {
		t.Fatal(err)
	}
	evidence, err := ParseResourceEvidence(strings.NewReader(completeResourceEvidence))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteResourceEvidence(context.Background(), metadata, evidence); err != nil {
		t.Fatal(err)
	}
	record, err := store.FindByRunner(context.Background(), metadata.PoolID, metadata.WorkerName)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "logs"), filepath.Join(root, "diag"), record.LogPath, record.DiagnosticPath, record.ResourcePath} {
		if !acl.wasHardened(path) || !acl.wasVerified(path) {
			t.Fatalf("path %q ACL calls: hardened=%t verified=%t", path, acl.wasHardened(path), acl.wasVerified(path))
		}
	}
}

func TestResourceEvidenceIsDurableAndRequiredBeforeFinalization(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newTestJobStore(t, filepath.Join(root, "state"))
	sink := newArtifactSinkForTest(t, root, store, defaultArtifactPolicy())
	metadata := testArtifactMetadata("container-resources", "runner-resources")
	writer, err := sink.OpenLog(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteDiagnostics(context.Background(), metadata, strings.NewReader("diagnostics")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(context.Background(), metadata); err == nil {
		t.Fatal("worker finalized without terminal resource evidence")
	}
	evidence, err := ParseResourceEvidence(strings.NewReader(completeResourceEvidence))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteResourceEvidence(context.Background(), metadata, evidence); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	record, err := store.FindByRunner(context.Background(), metadata.PoolID, metadata.WorkerName)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(record.ResourcePath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseResourceEvidence(bytes.NewReader(content))
	if err != nil || parsed.Memory.PeakBytes != evidence.Memory.PeakBytes || record.Open || record.FinalizedAt.IsZero() {
		t.Fatalf("record=%#v evidence=%#v error=%v", record, parsed, err)
	}
}

func newArtifactSinkForTest(t *testing.T, root string, store jobindex.Store, policy ArtifactPolicy) *FileArtifactSink {
	t.Helper()
	sink, err := NewFileArtifactSink(filepath.Join(root, "logs"), filepath.Join(root, "diag"), store, testJobACL{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

func defaultArtifactPolicy() ArtifactPolicy {
	return ArtifactPolicy{
		MaxFileSizeBytes: 1 << 20, RawDiagnosticMaxInputBytes: 2 << 20,
		Retention: time.Hour, TotalCapBytes: 4 << 20, CleanupEvery: time.Minute,
	}
}

func testArtifactMetadata(containerID, runnerName string) ArtifactMetadata {
	return ArtifactMetadata{
		ContainerID: containerID, PoolID: "org", WorkerName: runnerName,
		StartedAt: time.Unix(1, 0).UTC(),
	}
}

type recordingArtifactACL struct {
	mu       sync.Mutex
	hardened map[string]bool
	verified map[string]bool
}

func (a *recordingArtifactACL) Harden(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hardened == nil {
		a.hardened = map[string]bool{}
	}
	a.hardened[filepath.Clean(path)] = true
	return nil
}

func (a *recordingArtifactACL) Verify(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verified == nil {
		a.verified = map[string]bool{}
	}
	a.verified[filepath.Clean(path)] = true
	return nil
}

func (a *recordingArtifactACL) wasHardened(path string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hardened[filepath.Clean(path)]
}

func (a *recordingArtifactACL) wasVerified(path string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.verified[filepath.Clean(path)]
}
