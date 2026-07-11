package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/secret"
)

const maximumDoctorACLEntries = 100_000

type doctorACLVerifier interface {
	Verify(string) error
}

type doctorSecretInspector interface {
	Inspect(context.Context, string) (secret.ImportResult, error)
}

type doctorEngineProbe func(context.Context) (string, string, error)

type LocalDoctorInspector struct {
	Config    config.Config
	ACL       doctorACLVerifier
	BitLocker secret.BitLockerVerifier
	Secrets   doctorSecretInspector
	Engine    doctorEngineProbe
	Now       func() time.Time
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
		Now: func() time.Time { return time.Now().UTC() },
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

	if i.BitLocker == nil {
		checks = append(checks, DoctorCheck{Name: "bitlocker", Healthy: false, Detail: "BitLocker verifier is unavailable"})
	} else if err := i.BitLocker.VerifyProtected(ctx, i.Config.Paths.Secrets); err != nil {
		checks = append(checks, DoctorCheck{Name: "bitlocker", Healthy: false, Detail: err.Error()})
	} else {
		checks = append(checks, DoctorCheck{Name: "bitlocker", Healthy: true, Detail: "secret volume is fully encrypted and protection is on"})
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
	orderedSecretIDs := make([]string, 0, len(secretIDs))
	for id := range secretIDs {
		orderedSecretIDs = append(orderedSecretIDs, id)
	}
	sort.Strings(orderedSecretIDs)
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
		metadata, err := i.Secrets.Inspect(ctx, id)
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
		operatingSystem, architecture, err := i.Engine(ctx)
		healthy := err == nil && operatingSystem == "linux" && (architecture == "amd64" || architecture == "x86_64")
		detail := fmt.Sprintf("fixed local endpoint reports %s/%s", displayValue(operatingSystem), displayValue(architecture))
		if err != nil {
			detail = err.Error()
		}
		checks = append(checks, DoctorCheck{Name: "local-docker-engine", Healthy: healthy, Detail: detail})
	}

	checks = append(checks, DoctorCheck{
		Name:    "github-jit-proof",
		Skipped: true,
		Detail:  "not performed by doctor because JIT creation mutates GitHub runner inventory; the live canary acceptance gate performs this proof",
	})
	return checks
}

func (i *LocalDoctorInspector) verifyACLTree(root string) (int, error) {
	if i.ACL == nil {
		return 0, errors.New("ACL verifier is unavailable")
	}
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
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
			return fmt.Errorf("verify %s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		return count, err
	}
	return count, nil
}

var _ DoctorInspector = (*LocalDoctorInspector)(nil)
