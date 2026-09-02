package statefs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newDurableWriteForTest builds a plan that appends every stage it reaches to
// stages, so a test can pin the order the shared sequence runs them in.
func newDurableWriteForTest(directory, target string, stages *[]string) DurableWrite {
	record := func(stage string) { *stages = append(*stages, stage) }
	return DurableWrite{
		Directory:        directory,
		Target:           target,
		TemporaryPattern: ".durable-*",
		Mode:             0o600,
		HardenBeforeWrite: func(string) error {
			record("harden-before-write")
			return nil
		},
		HardenBeforeReplace: func(string) error {
			record("harden-before-replace")
			return nil
		},
		BeforeReplace: func() error {
			record("before-replace")
			return nil
		},
		AfterReplace: func() error {
			record("after-replace")
			return nil
		},
		HardenAfterReplace: func(string) error {
			record("harden-after-replace")
			return nil
		},
		OnCommitFailure: func(string) { record("commit-failure") },
		Labels: DurableWriteLabels{
			CreateTemporary: "create temporary probe",
			SetMode:         "set temporary probe permissions",
			Write:           "write temporary probe",
			Flush:           "flush temporary probe",
			Close:           "close temporary probe",
			Replace:         "replace probe atomically",
			FlushDirectory:  "flush probe directory",
		},
	}
}

// TestDurableWriteRunsSeamsInOrder pins the sequence every migrated state
// writer now depends on: the temporary is hardened before content reaches it
// and again before the replace, the destination recheck lands immediately
// before the commit, and dependent sidecars are published between the replace
// and the hardening of the committed file.
func TestDurableWriteRunsSeamsInOrder(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "probe.json")
	var stages []string
	if err := newDurableWriteForTest(directory, target, &stages).WriteBytes([]byte("payload\n")); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	want := []string{"harden-before-write", "harden-before-replace", "before-replace", "after-replace", "harden-after-replace"}
	if !slices.Equal(stages, want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "payload\n" {
		t.Fatalf("committed contents = %q, want the written payload", contents)
	}
	if entries, err := os.ReadDir(directory); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 {
		t.Fatalf("directory entries = %v, want the committed file alone", entries)
	}
}

// TestDurableWriteRemovesTemporaryWhenAStageFails pins the cleanup contract
// each migrated writer relied on before the sequence was shared: a failure
// before the replace propagates the caller's error, removes the temporary, and
// leaves no committed file behind.
func TestDurableWriteRemovesTemporaryWhenAStageFails(t *testing.T) {
	t.Parallel()
	hardenFailure := errors.New("harden refused")
	for name, mutate := range map[string]func(*DurableWrite){
		"before write":   func(plan *DurableWrite) { plan.HardenBeforeWrite = func(string) error { return hardenFailure } },
		"before replace": func(plan *DurableWrite) { plan.HardenBeforeReplace = func(string) error { return hardenFailure } },
		"destination recheck": func(plan *DurableWrite) {
			plan.BeforeReplace = func() error { return hardenFailure }
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			var stages []string
			plan := newDurableWriteForTest(directory, filepath.Join(directory, "probe.json"), &stages)
			mutate(&plan)
			if err := plan.WriteBytes([]byte("payload\n")); !errors.Is(err, hardenFailure) {
				t.Fatalf("WriteBytes = %v, want the seam failure unwrapped", err)
			}
			if slices.Contains(stages, "commit-failure") {
				t.Fatalf("stages = %v, want no post-commit withdrawal before the replace", stages)
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("directory entries = %v, want the temporary removed and no committed file", entries)
			}
		})
	}
}

// TestDurableWriteWithdrawsTargetAfterPostCommitFailure pins the withdrawal
// seam terminal evidence depends on: a file that committed and then failed a
// later stage must not survive as a readable half-verified artifact.
func TestDurableWriteWithdrawsTargetAfterPostCommitFailure(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "probe.json")
	var stages []string
	plan := newDurableWriteForTest(directory, target, &stages)
	verifyFailure := errors.New("verify refused")
	plan.HardenAfterReplace = func(string) error { return verifyFailure }
	plan.OnCommitFailure = func(path string) { _ = os.Remove(path) }
	if err := plan.WriteBytes([]byte("payload\n")); !errors.Is(err, verifyFailure) {
		t.Fatalf("WriteBytes = %v, want the post-commit failure", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat committed target = %v, want it withdrawn", err)
	}
}

// TestDurableWriteLabelsFailedStages pins the operator-facing wording: a stage
// with a label reports "<label>: <cause>", and an unlabeled stage — what the
// drop journal's discarded writes use — leaves the cause unwrapped.
func TestDurableWriteLabelsFailedStages(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	var stages []string
	plan := newDurableWriteForTest(directory, filepath.Join(directory, "probe.json"), &stages)
	plan.Directory = filepath.Join(directory, "missing")
	err := plan.WriteBytes([]byte("payload\n"))
	if err == nil || !strings.HasPrefix(err.Error(), "create temporary probe: ") {
		t.Fatalf("WriteBytes = %v, want the create-temporary label", err)
	}
	plan.Labels.CreateTemporary = ""
	if err := plan.WriteBytes([]byte("payload\n")); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("WriteBytes = %v, want the unwrapped cause", err)
	}
}

// TestDurableWriteSkipsDirectorySyncWhenAsked pins the assign-times sidecar's
// arrangement: its enclosing save owns the one directory flush, so the sidecar
// must commit without flushing the directory itself.
func TestDurableWriteSkipsDirectorySyncWhenAsked(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "probe.json")
	var stages []string
	plan := newDurableWriteForTest(directory, target, &stages)
	plan.SkipDirectorySync = true
	plan.Directory = directory
	if err := plan.WriteBytes([]byte("payload\n")); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("stat committed target = %v, want the commit to have landed", err)
	}
	if slices.Contains(stages, "commit-failure") {
		t.Fatalf("stages = %v, want no withdrawal on the skipped flush", stages)
	}
}

// TestDurableWriterStreamsThroughCommit pins the streaming arrangement the
// worker log and the diagnostic archive use: the caller writes through the
// prepared temporary itself and Discard after Commit leaves the committed file
// in place.
func TestDurableWriterStreamsThroughCommit(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "probe.log")
	var stages []string
	writer, err := newDurableWriteForTest(directory, target, &stages).Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer writer.Discard()
	for _, chunk := range []string{"first\n", "second\n"} {
		if _, err := writer.File().Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	writer.Discard()
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "first\nsecond\n" {
		t.Fatalf("committed contents = %q, want both streamed chunks", contents)
	}
}

// TestDurableWriterDiscardRemovesAbandonedTemporary pins the abandoned-stream
// case: a caller that never commits must leave nothing on disk.
func TestDurableWriterDiscardRemovesAbandonedTemporary(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	var stages []string
	writer, err := newDurableWriteForTest(directory, filepath.Join(directory, "probe.log"), &stages).Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := writer.File().Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	writer.Discard()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory entries = %v, want the abandoned temporary removed", entries)
	}
}
