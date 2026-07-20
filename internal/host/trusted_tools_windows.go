//go:build windows

package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// resolveExecutable returns path when the caller explicitly configured one,
// otherwise resolves the trusted default via fallback.
func resolveExecutable(path string, fallback func() (string, error)) (string, error) {
	if path != "" {
		return path, nil
	}
	return fallback()
}

// resolvedCommandRunner returns runner, or a default ExecCommandRunner when
// the caller left the field unset. Host adapters expose a CommandRunner field
// so tests can substitute a fake without spawning real processes.
func resolvedCommandRunner(runner CommandRunner) CommandRunner {
	if runner == nil {
		return ExecCommandRunner{}
	}
	return runner
}

func trustedSystemExecutable(name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return "", errors.New("system executable name must be a base name")
	}
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return "", fmt.Errorf("resolve Windows system directory: %w", err)
	}
	return verifyTrustedExecutable(filepath.Join(systemDirectory, name))
}

func trustedDockerDesktopExecutable() (string, error) {
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("resolve Program Files: %w", err)
	}
	return verifyTrustedExecutable(filepath.Join(programFiles, "Docker", "Docker", "resources", "bin", "docker.exe"))
}

func verifyTrustedExecutable(path string) (string, error) {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	if !filepath.IsAbs(clean) || volume == "" || strings.HasPrefix(volume, `\\`) {
		return "", fmt.Errorf("trusted executable path %q is not an absolute local-drive path", clean)
	}
	current := volume + string(filepath.Separator)
	remainder := strings.TrimLeft(strings.TrimPrefix(clean, volume), `\/`)
	for _, component := range strings.FieldsFunc(remainder, func(value rune) bool { return value == '\\' || value == '/' }) {
		current = filepath.Join(current, component)
		pointer, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return "", fmt.Errorf("encode trusted executable component %q: %w", current, err)
		}
		attributes, err := windows.GetFileAttributes(pointer)
		if err != nil {
			return "", fmt.Errorf("inspect trusted executable component %q: %w", current, err)
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return "", fmt.Errorf("trusted executable component %q is a reparse point", current)
		}
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return "", fmt.Errorf("inspect trusted executable %q: %w", clean, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("trusted executable %q is not a regular file", clean)
	}
	return clean, nil
}
