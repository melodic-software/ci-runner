//go:build windows

package secret

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsPrivateKeySource requests read and delete access without sharing
// write or delete access. Per CreateFile's documented share-mode contract,
// the pathname therefore cannot be written, renamed, or deleted while this
// handle remains open. CommitRemoval marks this exact handle for deletion.
type windowsPrivateKeySource struct {
	path string
	file *os.File
	info os.FileInfo
}

func verifySourcePathIdentity(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("source pathname no longer identifies the opened file: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) {
		return errors.New("source pathname identity changed after the file was opened")
	}
	return nil
}

func openPrivateKeySource(path string) (privateKeySource, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect private-key source before locking: %w", err)
	}
	if err := inspectPrivateKeySource(before); err != nil {
		return nil, err
	}

	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode private-key source path: %w", err)
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.GENERIC_READ|windows.DELETE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("lock private-key source for read and identity-bound deletion: %w", err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("adopt private-key source handle")
	}
	fail := func(cause error) (privateKeySource, error) {
		if closeErr := file.Close(); closeErr != nil {
			return nil, errors.Join(cause, fmt.Errorf("close rejected private-key source: %w", closeErr))
		}
		return nil, cause
	}
	opened, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("inspect locked private-key source: %w", err))
	}
	if err := inspectPrivateKeySource(opened); err != nil {
		return fail(err)
	}
	if !os.SameFile(before, opened) {
		return fail(errors.New("private-key source identity changed while acquiring the lock"))
	}
	if err := verifySourcePathIdentity(path, opened); err != nil {
		return fail(err)
	}
	return &windowsPrivateKeySource{path: path, file: file, info: opened}, nil
}

func (s *windowsPrivateKeySource) Read(value []byte) (int, error) {
	if s.file == nil {
		return 0, os.ErrClosed
	}
	return s.file.Read(value)
}

func (s *windowsPrivateKeySource) CommitRemoval() error {
	if s.file == nil {
		return os.ErrClosed
	}
	if err := verifySourcePathIdentity(s.path, s.info); err != nil {
		return err
	}

	// FILE_DISPOSITION_INFO contains one Win32 BOOL. SetFileInformationByHandle
	// applies deletion to the file object represented by this handle rather
	// than performing a second pathname lookup.
	deleteFile := uint32(1)
	if err := windows.SetFileInformationByHandle(
		windows.Handle(s.file.Fd()),
		windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&deleteFile)),
		uint32(unsafe.Sizeof(deleteFile)),
	); err != nil {
		return fmt.Errorf("mark opened source handle for deletion: %w", err)
	}
	if err := s.close(); err != nil {
		return fmt.Errorf("close source handle after marking it for deletion: %w", err)
	}
	if _, err := os.Lstat(s.path); err == nil {
		return errors.New("source pathname still exists after identity-bound deletion; it may be a replacement")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("verify identity-bound source deletion: %w", err)
	}
	return nil
}

func (s *windowsPrivateKeySource) Close() error {
	return s.close()
}

func (s *windowsPrivateKeySource) close() error {
	if s.file == nil {
		return nil
	}
	file := s.file
	s.file = nil
	return file.Close()
}
