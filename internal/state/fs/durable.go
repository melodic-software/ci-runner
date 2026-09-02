package statefs

import (
	"fmt"
	"os"
)

// DurableWrite publishes one file through the sequence every durable writer in
// the tree shares: create a same-directory temporary, restrict its mode, write
// and flush the contents, ACL-harden them, replace the target atomically,
// harden the committed file, and flush the containing directory. Callers differ
// only in the seams below — the label each stage reports, which ACL passes run
// and when, and what happens around the replace — so the ordering itself is
// owned here instead of being restated once per state file, where a single
// transposed step silently costs crash safety or leaves a committed file with
// unprotected ACL inheritance.
type DurableWrite struct {
	// Directory holds the in-flight temporary and is flushed once the replace
	// commits. It must be on Target's volume for the replace to stay atomic.
	Directory string

	// Target is the committed path the temporary replaces.
	Target string

	// TemporaryPattern is the os.CreateTemp pattern for the in-flight file.
	// Artifact-root patterns must stay recognizable to the classifier that
	// protects in-flight files from the cap and orphan sweeps by name alone
	// (isInFlightTemporary in internal/runtime/docker/artifacts_audit.go).
	TemporaryPattern string

	// Mode is applied to the temporary before any content reaches it.
	Mode os.FileMode

	// HardenBeforeWrite, HardenBeforeReplace, and HardenAfterReplace run the
	// caller's ACL passes over the temporary (the first two) and the committed
	// target (the last). Each returns a fully formed error because callers
	// label harden and verify failures differently per pass, and a nil hook
	// skips that pass.
	HardenBeforeWrite   func(path string) error
	HardenBeforeReplace func(path string) error
	HardenAfterReplace  func(path string) error

	// BeforeReplace runs once the temporary is durable and hardened, for
	// callers that recheck the destination immediately before committing.
	BeforeReplace func() error

	// AfterReplace runs once the replace commits and before the target is
	// hardened, for callers that publish dependent sidecars under the same
	// lock as the file they belong to.
	AfterReplace func() error

	// OnCommitFailure withdraws the committed target when a stage after the
	// replace fails. Callers that would rather keep a committed file whose
	// post-commit verification failed leave it nil.
	OnCommitFailure func(target string)

	// SkipDirectorySync omits the closing directory flush, for callers whose
	// enclosing save flushes the same directory once for every file it wrote.
	SkipDirectorySync bool

	// Labels supply the error prefix each stage reports.
	Labels DurableWriteLabels
}

// DurableWriteLabels names the stages a DurableWrite can fail in. A stage
// reports "<label>: <cause>", and an empty label leaves the cause unwrapped —
// which is what a caller that deliberately discards every failure wants.
type DurableWriteLabels struct {
	CreateTemporary string
	SetMode         string
	Write           string
	Flush           string
	Close           string
	Replace         string
	FlushDirectory  string
}

// WriteBytes runs the whole sequence for a payload the caller already holds.
func (plan DurableWrite) WriteBytes(contents []byte) error {
	writer, err := plan.Begin()
	if err != nil {
		return err
	}
	defer writer.Discard()
	if _, err := writer.File().Write(contents); err != nil {
		return labelError(plan.Labels.Write, err)
	}
	return writer.Commit()
}

// Begin creates the temporary, restricts its mode, and runs the before-write
// ACL pass. The caller must Discard the writer — a no-op once Commit published
// the file — so an abandoned temporary never survives.
func (plan DurableWrite) Begin() (*DurableWriter, error) {
	file, err := os.CreateTemp(plan.Directory, plan.TemporaryPattern)
	if err != nil {
		return nil, labelError(plan.Labels.CreateTemporary, err)
	}
	writer := &DurableWriter{plan: plan, file: file, temporaryPath: file.Name()}
	if err := file.Chmod(plan.Mode); err != nil {
		writer.Discard()
		return nil, labelError(plan.Labels.SetMode, err)
	}
	if plan.HardenBeforeWrite != nil {
		if err := plan.HardenBeforeWrite(writer.temporaryPath); err != nil {
			writer.Discard()
			return nil, err
		}
	}
	return writer, nil
}

// DurableWriter is a prepared temporary whose contents the caller streams
// itself — a compressed archive, or a worker log that stays open for the
// container's lifetime — before Commit publishes it. Callers holding a
// complete payload should use DurableWrite.WriteBytes instead.
type DurableWriter struct {
	plan          DurableWrite
	file          *os.File
	temporaryPath string
	committed     bool
}

// File is the open temporary the caller writes through before Commit.
func (w *DurableWriter) File() *os.File { return w.file }

// Discard closes the temporary and removes it unless Commit already published
// it. It is safe after a successful Commit and safe to repeat.
func (w *DurableWriter) Discard() {
	_ = w.file.Close()
	if !w.committed {
		_ = os.Remove(w.temporaryPath)
	}
}

// Commit flushes and closes the temporary, hardens it, replaces the target
// atomically, hardens the committed file, and flushes the directory.
func (w *DurableWriter) Commit() error {
	if err := w.file.Sync(); err != nil {
		return labelError(w.plan.Labels.Flush, err)
	}
	if err := w.file.Close(); err != nil {
		return labelError(w.plan.Labels.Close, err)
	}
	if w.plan.HardenBeforeReplace != nil {
		if err := w.plan.HardenBeforeReplace(w.temporaryPath); err != nil {
			return err
		}
	}
	if w.plan.BeforeReplace != nil {
		if err := w.plan.BeforeReplace(); err != nil {
			return err
		}
	}
	if err := atomicReplace(w.temporaryPath, w.plan.Target); err != nil {
		return labelError(w.plan.Labels.Replace, err)
	}
	w.committed = true
	if w.plan.AfterReplace != nil {
		if err := w.plan.AfterReplace(); err != nil {
			return w.withdraw(err)
		}
	}
	if w.plan.HardenAfterReplace != nil {
		if err := w.plan.HardenAfterReplace(w.plan.Target); err != nil {
			return w.withdraw(err)
		}
	}
	if w.plan.SkipDirectorySync {
		return nil
	}
	if err := syncDirectory(w.plan.Directory); err != nil {
		return w.withdraw(labelError(w.plan.Labels.FlushDirectory, err))
	}
	return nil
}

// withdraw runs the caller's post-commit cleanup for a target that committed
// and then failed a later stage.
func (w *DurableWriter) withdraw(err error) error {
	if w.plan.OnCommitFailure != nil {
		w.plan.OnCommitFailure(w.plan.Target)
	}
	return err
}

func labelError(label string, err error) error {
	if label == "" {
		return err
	}
	return fmt.Errorf("%s: %w", label, err)
}
