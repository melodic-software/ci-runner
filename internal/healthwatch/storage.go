package healthwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrJobsFileMissing = errors.New("jobs.json is missing")

type OSJobsFile struct {
	Path string
}

func (f OSJobsFile) Size(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	info, err := os.Stat(f.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrJobsFileMissing
		}
		return 0, err
	}
	if info.IsDir() {
		return 0, fmt.Errorf("%q is a directory", f.Path)
	}
	return info.Size(), nil
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
