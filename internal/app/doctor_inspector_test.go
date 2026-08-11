package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/host"
)

type stubDoctorACLVerifier struct {
	verify func(path string) error
}

func (s stubDoctorACLVerifier) Verify(path string) error {
	if s.verify != nil {
		return s.verify(path)
	}
	return nil
}

type recordingDoctorBitLocker struct {
	calls int
	err   error
}

func (v *recordingDoctorBitLocker) VerifyProtected(context.Context, string) error {
	v.calls++
	return v.err
}

func TestLocalDoctorInspectorSkipsBitLockerByDefaultWithoutCallingVerifier(t *testing.T) {
	t.Parallel()
	verifier := &recordingDoctorBitLocker{err: errors.New("must not be called")}
	inspector := &LocalDoctorInspector{BitLocker: verifier}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{}), "bitlocker")

	if verifier.calls != 0 {
		t.Fatalf("default doctor invoked the UAC-capable BitLocker verifier %d time(s)", verifier.calls)
	}
	if !check.Skipped || !strings.Contains(check.Detail, "--include-elevated") {
		t.Fatalf("default BitLocker check = %#v, want an explicit non-failing skip", check)
	}
}

func TestLocalDoctorInspectorRunsAndEnforcesBitLockerWhenExplicitlyIncluded(t *testing.T) {
	t.Parallel()
	verifier := &recordingDoctorBitLocker{err: errors.New("protection is off")}
	inspector := &LocalDoctorInspector{BitLocker: verifier}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{IncludeElevated: true}), "bitlocker")

	if verifier.calls != 1 {
		t.Fatalf("opted-in doctor invoked the BitLocker verifier %d time(s), want 1", verifier.calls)
	}
	if check.Skipped || check.Healthy || !strings.Contains(check.Detail, "protection is off") {
		t.Fatalf("opted-in BitLocker failure was weakened: %#v", check)
	}
}

func TestLocalDoctorInspectorReportsPendingRebootAsNonFailingAdvisory(t *testing.T) {
	t.Parallel()
	inspector := &LocalDoctorInspector{PendingReboot: func() (host.PendingReboot, error) {
		return host.PendingReboot{ComponentServicing: true, WindowsUpdate: true}, nil
	}}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{}), "pending-os-reboot")

	if !check.Advisory || check.Healthy || check.Skipped {
		t.Fatalf("pending reboot check = %#v, want an unhealthy non-skipped advisory", check)
	}
	if !strings.Contains(check.Detail, "component-servicing") || !strings.Contains(check.Detail, "windows-update") {
		t.Fatalf("pending reboot detail %q does not name the fired signals", check.Detail)
	}
}

func TestLocalDoctorInspectorPassesPendingRebootWhenNoSignalFires(t *testing.T) {
	t.Parallel()
	inspector := &LocalDoctorInspector{PendingReboot: func() (host.PendingReboot, error) {
		return host.PendingReboot{}, nil
	}}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{}), "pending-os-reboot")

	if !check.Healthy || !check.Advisory || check.Skipped {
		t.Fatalf("clean pending reboot check = %#v, want a healthy advisory", check)
	}
}

func TestLocalDoctorInspectorKeepsPendingRebootProbeErrorsAdvisory(t *testing.T) {
	t.Parallel()
	inspector := &LocalDoctorInspector{PendingReboot: func() (host.PendingReboot, error) {
		return host.PendingReboot{FileRenameOperations: true}, errors.New("windows update: access denied")
	}}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{}), "pending-os-reboot")

	if !check.Advisory || check.Healthy || check.Skipped {
		t.Fatalf("partially failed pending reboot check = %#v, want an unhealthy advisory", check)
	}
	if !strings.Contains(check.Detail, "pending-file-renames") || !strings.Contains(check.Detail, "access denied") {
		t.Fatalf("pending reboot detail %q hides the fired signal or the probe failure", check.Detail)
	}
}

func TestLocalDoctorInspectorSkipsPendingRebootOffWindows(t *testing.T) {
	t.Parallel()
	inspector := &LocalDoctorInspector{PendingReboot: func() (host.PendingReboot, error) {
		return host.PendingReboot{}, host.ErrPendingRebootUnsupported
	}}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{}), "pending-os-reboot")

	if !check.Skipped || !strings.Contains(check.Detail, "Windows") {
		t.Fatalf("unsupported pending reboot check = %#v, want an explicit non-failing skip", check)
	}
}

func TestVerifyACLTreeIgnoresVanishedChildDuringProbe(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := filepath.Join(root, "observed.json")
	if err := os.WriteFile(child, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	verifier := stubDoctorACLVerifier{verify: func(path string) error {
		if path == child {
			return fmt.Errorf("inspect ACL target: %w", fs.ErrNotExist)
		}
		return nil
	}}
	inspector := &LocalDoctorInspector{ACL: verifier}

	count, err := inspector.verifyACLTree(root)
	if err != nil {
		t.Fatalf("verifyACLTree() err = %v, want nil", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 (root and listed child)", count)
	}
}

func TestVerifyACLTreeFailsOnRealACLFault(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := filepath.Join(root, "jobs.json")
	if err := os.WriteFile(child, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	verifier := stubDoctorACLVerifier{verify: func(path string) error {
		if path == child {
			return fmt.Errorf("ACL has 3 entries; expected exactly current user and SYSTEM")
		}
		return nil
	}}
	inspector := &LocalDoctorInspector{ACL: verifier}

	_, err := inspector.verifyACLTree(root)
	if err == nil || !strings.Contains(err.Error(), "verify "+child) {
		t.Fatalf("verifyACLTree() err = %v, want a wrapped ACL fault for the child", err)
	}
}

func TestVerifyACLTreeFailsOnMissingRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "missing-state")
	inspector := &LocalDoctorInspector{ACL: stubDoctorACLVerifier{}}

	_, err := inspector.verifyACLTree(root)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("verifyACLTree() err = %v, want fs.ErrNotExist for a missing root", err)
	}
}

func TestVerifyACLTreeIgnoresVanishedChildDuringWalk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	childDir := filepath.Join(root, "child")
	if err := os.Mkdir(childDir, 0o700); err != nil {
		t.Fatal(err)
	}
	verifier := stubDoctorACLVerifier{verify: func(path string) error {
		if path == childDir {
			if err := os.Remove(childDir); err != nil {
				t.Fatalf("remove vanished child directory: %v", err)
			}
			return fs.ErrNotExist
		}
		return nil
	}}
	inspector := &LocalDoctorInspector{ACL: verifier}

	count, err := inspector.verifyACLTree(root)
	if err != nil {
		t.Fatalf("verifyACLTree() err = %v, want nil after child vanished between listing and probe", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 (root and listed child)", count)
	}
}

func TestLocalDoctorInspectorACLStateCheckStaysHealthyWhenChildVanishesDuringProbe(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := filepath.Join(root, ".observed.json-tmp")
	if err := os.WriteFile(child, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	verifier := stubDoctorACLVerifier{verify: func(path string) error {
		if path == child {
			return fmt.Errorf("inspect ACL target: %w", fs.ErrNotExist)
		}
		return nil
	}}
	inspector := &LocalDoctorInspector{
		Config: config.Config{Paths: config.Paths{State: root}},
		ACL:    verifier,
	}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{}), "acl/state")
	if !check.Healthy {
		t.Fatalf("acl/state check = %#v, want healthy when a listed child vanishes before ACL probe", check)
	}
	if !strings.Contains(check.Detail, "(2 entries)") {
		t.Fatalf("acl/state detail %q, want unchanged entry-count semantics", check.Detail)
	}
}

func doctorCheckNamed(t *testing.T, checks []DoctorCheck, name string) DoctorCheck {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("doctor check %q not found in %#v", name, checks)
	return DoctorCheck{}
}
