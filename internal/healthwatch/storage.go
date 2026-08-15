package healthwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/melodic-software/ci-runner/internal/jobindex"
)

var (
	ErrJobsFileMissing = errors.New("jobs.json is missing")
	// ErrJobsFileUndecodable wraps a snapshot that read fine but failed the
	// strict catalog decode; the checker reports it as a finding rather than
	// an infrastructure error, because a corrupt index is exactly what the
	// watchdog exists to surface.
	ErrJobsFileUndecodable = errors.New("jobs.json is undecodable")
)

type OSJobsFile struct {
	Path string
}

func (f OSJobsFile) Snapshot(ctx context.Context) (JobsSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return JobsSnapshot{}, err
	}
	// One read yields both size and records, so the two can never disagree
	// about which version of the atomically-replaced file was observed.
	contents, err := os.ReadFile(f.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
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

type FileSidecar struct {
	Path string
}

func NewFileSidecar(path string) FileSidecar {
	return FileSidecar{Path: path}
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

func (s FileSidecar) Save(ctx context.Context, sidecar Sidecar) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create health watch sidecar directory: %w", err)
	}
	encoded, err := json.Marshal(sidecar)
	if err != nil {
		return fmt.Errorf("encode health watch sidecar: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.Path), ".health-watch-*")
	if err != nil {
		return fmt.Errorf("create temporary health watch sidecar: %w", err)
	}
	tempPath := temporary.Name()
	cleanup := func() { _ = os.Remove(tempPath) }
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		cleanup()
		return fmt.Errorf("write temporary health watch sidecar: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temporary health watch sidecar: %w", err)
	}
	if err := os.Rename(tempPath, s.Path); err != nil {
		cleanup()
		return fmt.Errorf("replace health watch sidecar atomically: %w", err)
	}
	return nil
}
