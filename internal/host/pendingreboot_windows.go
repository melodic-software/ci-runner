//go:build windows

package host

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// ProbePendingReboot reads the three pending-reboot signals without elevation:
// the Component Based Servicing RebootPending key, the Session Manager pending
// file-rename operations, and the Windows Update Auto Update RebootRequired
// key. Signals that fired are reported even when another signal's read failed,
// so a partial registry problem cannot hide a pending reboot.
func ProbePendingReboot() (PendingReboot, error) {
	var result PendingReboot
	var problems []error

	componentServicing, err := localMachineKeyExists(`SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending`)
	if err != nil {
		problems = append(problems, fmt.Errorf("component servicing: %w", err))
	}
	result.ComponentServicing = componentServicing

	fileRenames, err := sessionManagerHasPendingFileRenames()
	if err != nil {
		problems = append(problems, fmt.Errorf("session manager: %w", err))
	}
	result.FileRenameOperations = fileRenames

	windowsUpdate, err := localMachineKeyExists(`SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired`)
	if err != nil {
		problems = append(problems, fmt.Errorf("windows update: %w", err))
	}
	result.WindowsUpdate = windowsUpdate

	return result, errors.Join(problems...)
}

func localMachineKeyExists(path string) (bool, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, key.Close()
}

func sessionManagerHasPendingFileRenames() (bool, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager`, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = key.Close() }()
	for _, value := range []string{"PendingFileRenameOperations", "PendingFileRenameOperations2"} {
		entries, _, err := key.GetStringsValue(value)
		if errors.Is(err, registry.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if len(entries) > 0 {
			return true, nil
		}
	}
	return false, nil
}
