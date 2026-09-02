package healthwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/melodic-software/ci-runner/internal/jobindex"
	statefs "github.com/melodic-software/ci-runner/internal/state/fs"
)

var (
	ErrJobsFileMissing = errors.New("jobs.json is missing")
	// ErrJobsFileUndecodable wraps a snapshot that read fine but failed the
	// strict catalog decode; the checker reports it as a finding rather than
	// an infrastructure error, because a corrupt index is exactly what the
	// watchdog exists to surface.
	ErrJobsFileUndecodable = errors.New("jobs.json is undecodable")
)

// JobsBytesSource is the store surface the watchdog reads jobs.json through:
// one committed document per call, taken under the store's cross-process
// lock (jobindex.FileStore.SnapshotBytes). The watchdog must never open the
// file itself — an out-of-lock handle makes the controller's atomic replace
// fail with a sharing violation on Windows.
type JobsBytesSource interface {
	SnapshotBytes(context.Context) ([]byte, error)
}

type StoreJobsFile struct {
	Source JobsBytesSource
}

func (f StoreJobsFile) Snapshot(ctx context.Context) (JobsSnapshot, error) {
	// One locked read yields both size and records, so the two can never
	// disagree about which committed version of the file was observed.
	contents, err := f.Source.SnapshotBytes(ctx)
	if err != nil {
		if errors.Is(err, jobindex.ErrNotFound) {
			return JobsSnapshot{}, ErrJobsFileMissing
		}
		return JobsSnapshot{}, err
	}
	catalog, err := jobindex.DecodeCatalog(contents)
	if err != nil {
		return JobsSnapshot{}, fmt.Errorf("%w: %v", ErrJobsFileUndecodable, err)
	}
	return JobsSnapshot{SizeBytes: int64(len(contents)), Catalog: catalog}, nil
}

// AccessController hardens and verifies state-file ACLs. It is the same
// controller every other state-directory writer uses
// (secret.NewAccessController): on Windows a protected-inheritance DACL of
// SYSTEM plus the current user, elsewhere 0700/0600 permissions.
type AccessController interface {
	Harden(string) error
	Verify(string) error
}

type FileSidecar struct {
	Path string
	ACL  AccessController
}

func NewFileSidecar(path string, acl AccessController) (FileSidecar, error) {
	if acl == nil {
		return FileSidecar{}, errors.New("health watch sidecar requires an access controller")
	}
	return FileSidecar{Path: path, ACL: acl}, nil
}

// EnsureHardened converges an existing sidecar file onto the ACL a current
// Save commits. Saves before #269 left the file with unprotected inheritance,
// and no later save is guaranteed to correct it — the sidecar only changes on
// divergence transitions and alert receipts — so each check converges the
// file instead of leaving doctor's acl/state gate failing until an organic
// save lands. A missing file needs nothing.
func (s FileSidecar) EnsureHardened(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(s.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect health watch sidecar: %w", err)
	}
	if err := s.ACL.Harden(s.Path); err != nil {
		return fmt.Errorf("secure health watch sidecar ACL: %w", err)
	}
	if err := s.ACL.Verify(s.Path); err != nil {
		return fmt.Errorf("verify health watch sidecar ACL: %w", err)
	}
	return nil
}

func (s FileSidecar) Load(ctx context.Context) (Sidecar, error) {
	if err := ctx.Err(); err != nil {
		return Sidecar{}, err
	}
	contents, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Sidecar{}, nil
		}
		return Sidecar{}, fmt.Errorf("read health watch sidecar: %w", err)
	}
	if len(contents) == 0 {
		return Sidecar{}, nil
	}
	var sidecar Sidecar
	if err := json.Unmarshal(contents, &sidecar); err != nil {
		return Sidecar{}, fmt.Errorf("decode health watch sidecar: %w", err)
	}
	return sidecar, nil
}

// Save writes the sidecar with the same hardening sequence the jobindex and
// statefs writers use: the temporary file is chmodded, ACL-hardened, and
// verified before the atomic replace, so no committed version of the file
// ever carries unprotected inheritance — a bare rename would replace a
// hardened file with an inheritance-unprotected one on every save and fail
// doctor's acl/state check.
func (s FileSidecar) Save(ctx context.Context, sidecar Sidecar) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory := filepath.Dir(s.Path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create health watch sidecar directory: %w", err)
	}
	if err := s.ACL.Harden(directory); err != nil {
		return fmt.Errorf("secure health watch sidecar directory: %w", err)
	}
	if err := s.ACL.Verify(directory); err != nil {
		return fmt.Errorf("verify health watch sidecar directory ACL: %w", err)
	}
	encoded, err := json.Marshal(sidecar)
	if err != nil {
		return fmt.Errorf("encode health watch sidecar: %w", err)
	}
	return statefs.DurableWrite{
		Directory:        directory,
		Target:           s.Path,
		TemporaryPattern: ".health-watch-*",
		Mode:             0o600,
		HardenBeforeReplace: func(path string) error {
			if err := s.ACL.Harden(path); err != nil {
				return fmt.Errorf("secure temporary health watch sidecar ACL: %w", err)
			}
			if err := s.ACL.Verify(path); err != nil {
				return fmt.Errorf("verify temporary health watch sidecar ACL: %w", err)
			}
			return nil
		},
		HardenAfterReplace: func(path string) error {
			if err := s.ACL.Harden(path); err != nil {
				return fmt.Errorf("secure health watch sidecar ACL: %w", err)
			}
			if err := s.ACL.Verify(path); err != nil {
				return fmt.Errorf("verify health watch sidecar ACL: %w", err)
			}
			return nil
		},
		Labels: statefs.DurableWriteLabels{
			CreateTemporary: "create temporary health watch sidecar",
			SetMode:         "secure temporary health watch sidecar",
			Write:           "write temporary health watch sidecar",
			Flush:           "flush temporary health watch sidecar",
			Close:           "close temporary health watch sidecar",
			Replace:         "replace health watch sidecar atomically",
			FlushDirectory:  "flush health watch sidecar directory",
		},
	}.WriteBytes(encoded)
}
