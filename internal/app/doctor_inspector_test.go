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
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/host"
	"github.com/melodic-software/ci-runner/internal/secret"
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

type deadlineRecordingBitLocker struct {
	budget      time.Duration
	hadDeadline bool
	block       bool
}

func (v *deadlineRecordingBitLocker) VerifyProtected(ctx context.Context, _ string) error {
	if deadline, ok := ctx.Deadline(); ok {
		v.hadDeadline = true
		v.budget = time.Until(deadline)
	}
	if v.block {
		<-ctx.Done()
		return context.Cause(ctx)
	}
	return nil
}

type deadlineRecordingSecrets struct {
	budget     time.Duration
	importedAt time.Time
}

func (s *deadlineRecordingSecrets) Inspect(ctx context.Context, _ string) (secret.ImportResult, error) {
	if deadline, ok := ctx.Deadline(); ok {
		s.budget = time.Until(deadline)
	}
	if err := ctx.Err(); err != nil {
		return secret.ImportResult{}, err
	}
	return secret.ImportResult{Fingerprint: "sha256:fixture", ImportedAt: s.importedAt}, nil
}

type deadlineRecordingEngine struct {
	budget time.Duration
}

func (e *deadlineRecordingEngine) probe(ctx context.Context) (string, string, error) {
	if deadline, ok := ctx.Deadline(); ok {
		e.budget = time.Until(deadline)
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	return "linux", "amd64", nil
}

func TestLocalDoctorInspectorBudgetsTheElevatedProbeForHumanSpeed(t *testing.T) {
	t.Parallel()
	const machineBudget = 15 * time.Second
	verifier := &deadlineRecordingBitLocker{}
	inspector := &LocalDoctorInspector{
		Config:    config.Config{Controller: config.Controller{LocalProbeTimeout: config.Duration{Duration: machineBudget}}},
		BitLocker: verifier,
	}

	doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{IncludeElevated: true}), "bitlocker")

	if !verifier.hadDeadline {
		t.Fatal("elevated BitLocker probe ran with no deadline at all")
	}
	if verifier.budget <= machineBudget {
		t.Fatalf("elevated probe budget = %s, want more than the %s machine-probe budget", verifier.budget, machineBudget)
	}
	if verifier.budget < defaultElevatedProbeTimeout-time.Minute {
		t.Fatalf("elevated probe budget = %s, want the %s default when none is configured", verifier.budget, defaultElevatedProbeTimeout)
	}
}

func TestLocalDoctorInspectorHonorsTheConfiguredElevatedProbeBudget(t *testing.T) {
	t.Parallel()
	const configured = 7 * time.Minute
	verifier := &deadlineRecordingBitLocker{}
	inspector := &LocalDoctorInspector{
		Config: config.Config{Controller: config.Controller{
			LocalProbeTimeout:    config.Duration{Duration: 15 * time.Second},
			ElevatedProbeTimeout: config.Duration{Duration: configured},
		}},
		BitLocker: verifier,
	}

	doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{IncludeElevated: true}), "bitlocker")

	if verifier.budget < configured-time.Minute || verifier.budget > configured {
		t.Fatalf("elevated probe budget = %s, want the configured %s", verifier.budget, configured)
	}
}

func TestLocalDoctorInspectorKeepsMachineChecksOffTheElevatedProbeBudget(t *testing.T) {
	t.Parallel()
	const machineBudget = 30 * time.Second
	secrets := &deadlineRecordingSecrets{importedAt: time.Now().UTC().Add(-time.Hour)}
	engine := &deadlineRecordingEngine{}
	inspector := &LocalDoctorInspector{
		Config: config.Config{
			Controller: config.Controller{
				LocalProbeTimeout:    config.Duration{Duration: machineBudget},
				ElevatedProbeTimeout: config.Duration{Duration: 10 * time.Millisecond},
			},
			GitHub: config.GitHub{Targets: []config.Target{{ID: "organization", SecretID: "melodic-software-host"}}},
		},
		BitLocker: &deadlineRecordingBitLocker{block: true},
		Secrets:   secrets,
		Engine:    engine.probe,
	}

	checks := inspector.Inspect(context.Background(), DoctorInspection{IncludeElevated: true, CheckDocker: true})

	if check := doctorCheckNamed(t, checks, "bitlocker"); check.Healthy {
		t.Fatalf("elevated probe that never answered = %#v, want unhealthy", check)
	}
	if check := doctorCheckNamed(t, checks, "credential/melodic-software-host"); !check.Healthy {
		t.Fatalf("credential check = %#v, want its real healthy observation", check)
	}
	if check := doctorCheckNamed(t, checks, "local-docker-engine"); !check.Healthy || !strings.Contains(check.Detail, "linux/amd64") {
		t.Fatalf("docker engine check = %#v, want its real healthy observation", check)
	}
	if secrets.budget < machineBudget-time.Second || engine.budget < machineBudget-time.Second {
		t.Fatalf("machine probes ran on %s and %s of budget, want their own %s", secrets.budget, engine.budget, machineBudget)
	}
}

func TestLocalDoctorInspectorReportsAnExpiredElevatedProbeAsItsOwnDeadline(t *testing.T) {
	t.Parallel()
	inspector := &LocalDoctorInspector{
		Config:    config.Config{Controller: config.Controller{ElevatedProbeTimeout: config.Duration{Duration: 10 * time.Millisecond}}},
		BitLocker: &deadlineRecordingBitLocker{block: true},
	}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{IncludeElevated: true}), "bitlocker")

	if check.Healthy || !strings.Contains(check.Detail, "did not complete within its deadline") {
		t.Fatalf("expired elevated probe = %#v, want the expired deadline named", check)
	}
	if strings.Contains(check.Detail, "declined") {
		t.Fatalf("expired deadline reported as a declined UAC prompt: %q", check.Detail)
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

func TestVerifyACLTreeFailsWhenRootVanishesAfterWalkBegins(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	verifier := stubDoctorACLVerifier{verify: func(path string) error {
		if path == root {
			return fmt.Errorf("inspect ACL target: %w", fs.ErrNotExist)
		}
		return nil
	}}
	inspector := &LocalDoctorInspector{ACL: verifier}

	_, err := inspector.verifyACLTree(root)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("verifyACLTree() err = %v, want fs.ErrNotExist when root vanishes before ACL probe", err)
	}
}

func TestLocalDoctorInspectorACLStateCheckFailsWhenRootVanishesDuringProbe(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	verifier := stubDoctorACLVerifier{verify: func(path string) error {
		if path == root {
			return fmt.Errorf("inspect ACL target: %w", fs.ErrNotExist)
		}
		return nil
	}}
	inspector := &LocalDoctorInspector{
		Config: config.Config{Paths: config.Paths{State: root}},
		ACL:    verifier,
	}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{}), "acl/state")
	if check.Healthy {
		t.Fatalf("acl/state check = %#v, want unhealthy when root vanishes before ACL probe", check)
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
