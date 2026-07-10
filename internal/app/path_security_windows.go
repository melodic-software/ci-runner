//go:build windows

package app

import (
	"os"

	"golang.org/x/sys/windows"
)

const fileAttributeReparsePoint = 0x00000400

func pathHasReparsePoint(path string, _ os.FileInfo) (bool, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(pointer)
	if err != nil {
		return false, err
	}
	return attributes&fileAttributeReparsePoint != 0, nil
}
