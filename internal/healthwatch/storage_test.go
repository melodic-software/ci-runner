package healthwatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingSidecarACL struct {
	mu       sync.Mutex
	hardened []string
	verified []string
}

func (a *recordingSidecarACL) Harden(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hardened = append(a.hardened, filepath.Clean(path))
	return nil
}

func (a *recordingSidecarACL) Verify(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.verified = append(a.verified, filepath.Clean(path))
	return nil
}

func (a *recordingSidecarACL) sawBoth(path string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	path = filepath.Clean(path)
	return containsSidecarPath(a.hardened, path) && containsSidecarPath(a.verified, path)
}

func (a *recordingSidecarACL) sawTemporary() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, path := range a.hardened {
		if strings.HasPrefix(filepath.Base(path), ".health-watch-") && containsSidecarPath(a.verified, path) {
			return true
		}
	}
	return false
}

func containsSidecarPath(paths []string, expected string) bool {
	for _, path := range paths {
		if path == expected {
			return true
		}
	}
	return false
}

func TestNewFileSidecarRequiresAccessController(t *testing.T) {
	t.Parallel()
	if _, err := NewFileSidecar(filepath.Join(t.TempDir(), "health-watch.json"), nil); err == nil {
		t.Fatal("expected missing access controller error")
	}
}

// TestFileSidecarSaveHardensEveryPathItWrites pins the #269 regression: every
// save must harden and verify the state directory, the temporary file before
// the atomic replace, and the committed file — the same sequence every other
// state-directory writer follows — so no committed version of
// health-watch.json ever carries unprotected ACL inheritance.
func TestFileSidecarSaveHardensEveryPathItWrites(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "health-watch.json")
	acl := &recordingSidecarACL{}
	sidecar, err := NewFileSidecar(path, acl)
	if err != nil {
		t.Fatal(err)
	}
	saved := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	if err := sidecar.Save(context.Background(), Sidecar{WorkerDivergenceSince: &saved}); err != nil {
		t.Fatal(err)
	}
	if !acl.sawBoth(directory) || !acl.sawBoth(path) || !acl.sawTemporary() {
		t.Fatalf("ACL calls hardened=%v verified=%v", acl.hardened, acl.verified)
	}
	loaded, err := sidecar.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorkerDivergenceSince == nil || !loaded.WorkerDivergenceSince.Equal(saved) {
		t.Fatalf("loaded = %#v, want the saved divergence timestamp", loaded)
	}
}

type failingSidecarACL struct {
	recordingSidecarACL
	failPrefix string
}

func (a *failingSidecarACL) Harden(path string) error {
	if strings.HasPrefix(filepath.Base(path), a.failPrefix) {
		return errors.New("harden refused")
	}
	return a.recordingSidecarACL.Harden(path)
}

// TestFileSidecarSaveRemovesTemporaryWhenHardeningFails pins the cleanup
// contract: a save that cannot harden its temporary file must propagate the
// error, remove the temporary, and leave the committed file untouched.
func TestFileSidecarSaveRemovesTemporaryWhenHardeningFails(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "health-watch.json")
	sidecar, err := NewFileSidecar(path, &failingSidecarACL{failPrefix: ".health-watch-"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sidecar.Save(context.Background(), Sidecar{LastAlertFingerprint: "fingerprint"}); err == nil {
		t.Fatal("expected temporary hardening failure to propagate")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory entries = %v, want the temporary removed and no committed file", entries)
	}
}

// TestFileSidecarEnsureHardenedConvergesExistingFile pins the #269 fleet
// remediation: a sidecar committed by a pre-fix release carries unprotected
// inheritance, and EnsureHardened must converge it without waiting for an
// organic save.
func TestFileSidecarEnsureHardenedConvergesExistingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "health-watch.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	acl := &recordingSidecarACL{}
	sidecar, err := NewFileSidecar(path, acl)
	if err != nil {
		t.Fatal(err)
	}
	if err := sidecar.EnsureHardened(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !acl.sawBoth(path) {
		t.Fatalf("ACL calls hardened=%v verified=%v", acl.hardened, acl.verified)
	}
}

func TestFileSidecarEnsureHardenedIgnoresMissingFile(t *testing.T) {
	t.Parallel()
	acl := &recordingSidecarACL{}
	sidecar, err := NewFileSidecar(filepath.Join(t.TempDir(), "health-watch.json"), acl)
	if err != nil {
		t.Fatal(err)
	}
	if err := sidecar.EnsureHardened(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(acl.hardened) != 0 || len(acl.verified) != 0 {
		t.Fatalf("ACL calls hardened=%v verified=%v, want none for a missing file", acl.hardened, acl.verified)
	}
}

func TestFileSidecarSaveReplacesExistingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "health-watch.json")
	sidecar, err := NewFileSidecar(path, &recordingSidecarACL{})
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	if err := sidecar.Save(context.Background(), Sidecar{WorkerDivergenceSince: &first}); err != nil {
		t.Fatal(err)
	}
	if err := sidecar.Save(context.Background(), Sidecar{LastAlertFingerprint: "fingerprint"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := sidecar.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorkerDivergenceSince != nil || loaded.LastAlertFingerprint != "fingerprint" {
		t.Fatalf("loaded = %#v, want the second save only", loaded)
	}
}
