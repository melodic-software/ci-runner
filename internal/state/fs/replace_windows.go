//go:build windows

package statefs

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
)

var (
	stateKernel32  = syscall.NewLazyDLL("kernel32.dll")
	procMoveFileEx = stateKernel32.NewProc("MoveFileExW")
)

func atomicReplace(source, target string) error {
	sourcePointer, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPointer, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	result, _, callErr := procMoveFileEx.Call(
		uintptr(unsafe.Pointer(sourcePointer)),
		uintptr(unsafe.Pointer(targetPointer)),
		moveFileReplaceExisting|moveFileWriteThrough,
	)
	if result == 0 {
		return fmt.Errorf("MoveFileExW: %w", windowsCallError(callErr))
	}
	return nil
}

// MoveFileExW with MOVEFILE_WRITE_THROUGH supplies the Windows durability
// boundary. Opening directories for fsync is neither portable nor required.
func syncDirectory(string) error { return nil }
