package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ensureNoReparsePoints(path string) error {
	return ensureNoReparsePointsUsing(path, pathHasReparsePoint)
}

func ensureNoReparsePointsUsing(path string, inspect func(string, os.FileInfo) (bool, error)) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return errors.New("runtime root must be absolute")
	}
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	remainder = strings.TrimLeft(remainder, `\/`)
	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	for _, component := range strings.FieldsFunc(remainder, func(value rune) bool { return value == '\\' || value == '/' }) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect runtime path component %q: %w", current, err)
		}
		reparse, err := inspect(current, info)
		if err != nil {
			return fmt.Errorf("inspect runtime path component %q for reparse data: %w", current, err)
		}
		if reparse {
			return fmt.Errorf("runtime path component %q is a symbolic link or reparse point", current)
		}
	}
	return nil
}

func preparePrivateRuntimeDirectory(directory string, acl runtimeAccessController) error {
	return preparePrivateRuntimeDirectoryUsing(directory, acl, ensureNoReparsePoints)
}

func preparePrivateRuntimeDirectoryUsing(directory string, acl runtimeAccessController, check func(string) error) error {
	if err := check(directory); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create private runtime directory %q: %w", directory, err)
	}
	if err := check(directory); err != nil {
		return err
	}
	if err := acl.Harden(directory); err != nil {
		return fmt.Errorf("secure private runtime directory %q: %w", directory, err)
	}
	if err := acl.Verify(directory); err != nil {
		return fmt.Errorf("verify private runtime directory %q: %w", directory, err)
	}
	return nil
}
