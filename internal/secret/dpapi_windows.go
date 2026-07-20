//go:build windows

package secret

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type DPAPIProtector struct {
	entropy []byte
}

func NewDPAPIProtector() DPAPIProtector {
	return DPAPIProtector{entropy: []byte("ci-runner/github-app-key/v1")}
}

func (p DPAPIProtector) Protect(plaintext []byte, description string) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, errors.New("cannot protect an empty secret")
	}
	input := blob(plaintext)
	entropy := blob(p.entropy)
	descriptionPointer, err := windows.UTF16PtrFromString(description)
	if err != nil {
		return nil, fmt.Errorf("encode DPAPI description: %w", err)
	}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, descriptionPointer, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	return copyAndFree(output, false)
}

func (p DPAPIProtector) Unprotect(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, errors.New("cannot unprotect an empty secret")
	}
	input := blob(ciphertext)
	entropy := blob(p.entropy)
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	return copyAndFree(output, true)
}

func blob(value []byte) windows.DataBlob {
	if len(value) == 0 {
		return windows.DataBlob{}
	}
	return windows.DataBlob{Size: uint32(len(value)), Data: &value[0]}
}

func copyAndFree(value windows.DataBlob, clearBeforeFree bool) ([]byte, error) {
	return copyAndFreeWith(value, clearBeforeFree, freeLocalMemory)
}

func copyAndFreeWith(value windows.DataBlob, clearBeforeFree bool, free func(uintptr) error) ([]byte, error) {
	if value.Data == nil {
		return nil, nil
	}
	var result []byte
	if value.Size > 0 {
		result = make([]byte, int(value.Size))
		unmanaged := unsafe.Slice(value.Data, int(value.Size))
		copy(result, unmanaged)
		if clearBeforeFree {
			for index := range unmanaged {
				unmanaged[index] = 0
			}
		}
	}
	if err := free(uintptr(unsafe.Pointer(value.Data))); err != nil {
		if clearBeforeFree {
			zero(result)
		}
		return nil, err
	}
	return result, nil
}

func freeLocalMemory(pointer uintptr) error {
	if _, err := windows.LocalFree(windows.Handle(pointer)); err != nil {
		return fmt.Errorf("free Windows local memory: %w", err)
	}
	return nil
}

func callError(err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return errors.New("call Windows API")
	}
	return err
}
