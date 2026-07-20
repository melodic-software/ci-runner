//go:build windows

package statefs

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func atomicReplace(source, target string) error {
	sourcePointer, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPointer, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(sourcePointer, targetPointer, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("MoveFileExW: %w", err)
	}
	return nil
}

// MoveFileExW with MOVEFILE_WRITE_THROUGH supplies the Windows durability
// boundary. Opening directories for fsync is neither portable nor required.
func syncDirectory(string) error { return nil }
