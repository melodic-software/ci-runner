package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/host"
	"github.com/melodic-software/ci-runner/internal/secret"
)

const maximumDoctorACLEntries = 100_000

// defaultElevatedProbeTimeout budgets the doctor's one elevated check on human
// time rather than machine time. It spans the whole sequence the operator sits
// through: the secure-desktop transition, a person noticing the prompt and
// answering it, an elevated Windows PowerShell starting, and the BitLocker
// module import plus CIM query behind Get-BitLockerVolume. The human term
// dominates and the machine terms are seconds, so this is deliberately far
// above controller.localProbeTimeout, which bounds probes no person
// participates in and stays short so a hung host is detected quickly.
// controller.elevatedProbeTimeout overrides it per host.
const defaultElevatedProbeTimeout = 2 * time.Minute

// errElevatedProbeTimedOut is the cause attached to the elevated probe's
// context. The bare DeadlineExceeded sentinel serves every deadline and so
// cannot say which one expired, and the probe's callee cannot see the doctor's
// budgets at all -- it reports whatever cause the context carries.
var errElevatedProbeTimedOut = errors.New("elevated host probe did not complete within its deadline, which spans answering the Administrator UAC prompt")

type doctorACLVerifier interface {
	Verify(string) error
}

type doctorSecretInspector interface {
	Inspect(context.Context, string) (secret.ImportResult, error)
}

type doctorEngineProbe func(context.Context) (string, string, error)

type LocalDoctorInspector struct {
	Config        config.Config
	ACL           doctorACLVerifier
	BitLocker     secret.BitLockerVerifier
	Secrets       doctorSecretInspector
	Engine        doctorEngineProbe
	PendingReboot func() (host.PendingReboot, error)
	Now           func() time.Time
}

func NewLocalDoctorInspector(
	cfg config.Config,
	acl doctorACLVerifier,
	bitLocker secret.BitLockerVerifier,
	secrets doctorSecretInspector,
	engine doctorEngineProbe,
) *LocalDoctorInspector {
	return &LocalDoctorInspector{
		Config: cfg, ACL: acl, BitLocker: bitLocker, Secrets: secrets, Engine: engine,
		PendingReboot: host.ProbePendingReboot,
		Now:           func() time.Time { return time.Now().UTC() },
	}
}

func (i *LocalDoctorInspector) Inspect(ctx context.Context, request DoctorInspection) []DoctorCheck {
	if i == nil {
		return []DoctorCheck{{Name: "host-security-and-runtime", Healthy: false, Detail: "local doctor inspector is nil"}}
	}
	checks := make([]DoctorCheck, 0, 12)

	manifest, err := LoadCompatibilityManifest(i.Config.Release.CompatibilityManifest, buildinfo.Version)
	if err != nil {
		checks = append(checks, DoctorCheck{Name: "compatibility-manifest", Healthy: false, Detail: err.Error()})
	} else {
		checks = append(checks, DoctorCheck{
			Name:    "compatibility-manifest",
			Healthy: true,
			Detail:  fmt.Sprintf("controller=%s source=%s worker=%s", manifest.Controller.Version, manifest.Source.SHA, manifest.WorkerReference()),
		})
	}

	aclRoots := []struct {
		name string
		path string
	}{
		{name: "secrets", path: i.Config.Paths.Secrets},
		{name: "state", path: i.Config.Paths.State},
		{name: "logs", path: i.Config.Paths.Logs},
		{name: "diagnostics", path: i.Config.Paths.Diagnostics},
	}
	for _, root := range aclRoots {
		count, err := i.verifyACLTree(root.path)
		detail := fmt.Sprintf("%s (%d entries)", root.path, count)
		if err != nil {
			detail += ": " + err.Error()
		}
		checks = append(checks, DoctorCheck{Name: "acl/" + root.name, Healthy: err == nil, Detail: detail})
	}

	secretIDs := make(map[string]struct{}, len(i.Config.GitHub.Targets))
	for _, target := range i.Config.GitHub.Targets {
		secretIDs[target.SecretID] = struct{}{}
	}
	orderedSecretIDs := slices.Sorted(maps.Keys(secretIDs))
	for _, id := range orderedSecretIDs {
		path := filepath.Join(i.Config.Paths.Secrets, id+".dpapi")
		if i.ACL == nil {
			checks = append(checks, DoctorCheck{Name: "credential-acl/" + id, Healthy: false, Detail: "ACL verifier is unavailable"})
		} else if err := i.ACL.Verify(path); err != nil {
			checks = append(checks, DoctorCheck{Name: "credential-acl/" + id, Healthy: false, Detail: err.Error()})
		} else {
			checks = append(checks, DoctorCheck{Name: "credential-acl/" + id, Healthy: true, Detail: "current user and SYSTEM only"})
		}

		if i.Secrets == nil {
			checks = append(checks, DoctorCheck{Name: "credential/" + id, Healthy: false, Detail: "secret inspector is unavailable"})
			continue
		}
		probeContext, cancelProbe := i.probe(ctx)
		metadata, err := i.Secrets.Inspect(probeContext, id)
		cancelProbe()
		if err != nil {
			checks = append(checks, DoctorCheck{Name: "credential/" + id, Healthy: false, Detail: err.Error()})
			continue
		}
		now := time.Now().UTC()
		if i.Now != nil {
			now = i.Now().UTC()
		}
		age := now.Sub(metadata.ImportedAt)
		healthy := !metadata.ImportedAt.IsZero() && age >= 0
		checks = append(checks, DoctorCheck{
			Name:    "credential/" + id,
			Healthy: healthy,
			Detail:  fmt.Sprintf("fingerprint=%s importedAt=%s age=%s", metadata.Fingerprint, metadata.ImportedAt.UTC().Format(time.RFC3339), age.Round(time.Second)),
		})
	}

	if !request.CheckDocker && !request.RequireDocker {
		checks = append(checks, DoctorCheck{Name: "local-docker-engine", Skipped: true, Detail: "not required in the current healthy lifecycle phase"})
	} else if i.Engine == nil {
		checks = append(checks, DoctorCheck{Name: "local-docker-engine", Healthy: false, Detail: "fixed-endpoint Docker Engine probe is unavailable"})
	} else {
		probeContext, cancelProbe := i.probe(ctx)
		operatingSystem, architecture, err := i.Engine(probeContext)
		cancelProbe()
		healthy := err == nil && operatingSystem == "linux" && (architecture == "amd64" || architecture == "x86_64")
		detail := fmt.Sprintf("fixed local endpoint reports %s/%s", displayValue(operatingSystem), displayValue(architecture))
		if err != nil {
			detail = err.Error()
		}
		checks = append(checks, DoctorCheck{Name: "local-docker-engine", Healthy: healthy, Detail: detail})
	}

	checks = append(checks, i.pendingRebootCheck())

	checks = append(checks, DoctorCheck{
		Name:    "github-jit-proof",
		Skipped: true,
		Detail:  "not performed by doctor because JIT creation mutates GitHub runner inventory; the first enable on a rolling-host rollout performs this proof under real traffic",
	})
	return append(checks, i.bitLockerCheck(ctx, request))
}

// bitLockerCheck runs last because it is the only check that can block on a
// person: every other result is computed and recorded before the UAC prompt
// takes the screen, so a slow or unanswered prompt costs nothing but its own
// row.
func (i *LocalDoctorInspector) bitLockerCheck(ctx context.Context, request DoctorInspection) DoctorCheck {
	switch {
	case !request.IncludeElevated:
		return DoctorCheck{
			Name:    "bitlocker",
			Skipped: true,
			Detail:  "requires an elevated BitLocker status probe that may open an Administrator UAC prompt; rerun with --include-elevated to perform it",
		}
	case i.BitLocker == nil:
		return DoctorCheck{Name: "bitlocker", Healthy: false, Detail: "BitLocker verifier is unavailable"}
	}
	probeContext, cancelProbe := i.elevatedProbe(ctx)
	defer cancelProbe()
	if err := i.BitLocker.VerifyProtected(probeContext, i.Config.Paths.Secrets); err != nil {
		return DoctorCheck{Name: "bitlocker", Healthy: false, Detail: err.Error()}
	}
	return DoctorCheck{Name: "bitlocker", Healthy: true, Detail: "secret volume is fully encrypted and protection is on"}
}

// probe derives one check's deadline from the caller's context so no check can
// spend another's budget: on a single shared deadline the first probe to
// exhaust it leaves every later check failing on a spent context without ever
// running.
func (i *LocalDoctorInspector) probe(ctx context.Context) (context.Context, context.CancelFunc) {
	return budgetedProbe(ctx, i.Config.Controller.LocalProbeTimeout.Duration, host.ErrProbeTimedOut)
}

func (i *LocalDoctorInspector) elevatedProbe(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := i.Config.Controller.ElevatedProbeTimeout.Duration
	if budget <= 0 {
		budget = defaultElevatedProbeTimeout
	}
	return budgetedProbe(ctx, budget, errElevatedProbeTimedOut)
}

func budgetedProbe(ctx context.Context, budget time.Duration, cause error) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeoutCause(ctx, budget, cause)
}

// pendingRebootCheck surfaces pending-OS-reboot state as advisory only: with
// updates auto-installed but never auto-rebooted while a session exists, a
// pending reboot is the expected standing residual the operator finishes
// during a deliberate drain window, not a fault.
func (i *LocalDoctorInspector) pendingRebootCheck() DoctorCheck {
	check := DoctorCheck{Name: "pending-os-reboot", Advisory: true}
	if i.PendingReboot == nil {
		check.Detail = "pending-reboot probe is unavailable"
		return check
	}
	pending, err := i.PendingReboot()
	if errors.Is(err, host.ErrPendingRebootUnsupported) {
		return DoctorCheck{Name: check.Name, Skipped: true, Detail: err.Error()}
	}
	switch {
	case pending.Pending():
		check.Detail = fmt.Sprintf("OS reboot pending (%s); expected under the never-auto-reboot update policy, finish it during the next deliberate drain window", strings.Join(pending.Signals(), ", "))
		if err != nil {
			check.Detail += "; partial probe failure: " + err.Error()
		}
	case err != nil:
		check.Detail = err.Error()
	default:
		check.Healthy = true
		check.Detail = "no reboot-pending signal from component servicing, session-manager file renames, or Windows Update"
	}
	return check
}

func (i *LocalDoctorInspector) verifyACLTree(root string) (int, error) {
	if i.ACL == nil {
		return 0, errors.New("ACL verifier is unavailable")
	}
	root = filepath.Clean(root)
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) && path != root {
				return nil
			}
			return walkErr
		}
		count++
		if count > maximumDoctorACLEntries {
			return fmt.Errorf("ACL verification exceeds the %d-entry safety limit", maximumDoctorACLEntries)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links and junction-like entries are not allowed in private runtime trees: %s", path)
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported filesystem entry in private runtime tree: %s (%s)", path, entry.Type())
		}
		if err := i.ACL.Verify(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) && path != root {
				return nil
			}
			return fmt.Errorf("verify %s: %w", path, err)
		}
		return nil
	})
	return count, err
}

var _ DoctorInspector = (*LocalDoctorInspector)(nil)
