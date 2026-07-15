//go:build windows

package secret

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

const cryptProtectUIForbidden = 0x1

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	Size uint32
	Data *byte
}

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
	descriptionPointer, err := syscall.UTF16PtrFromString(description)
	if err != nil {
		return nil, fmt.Errorf("encode DPAPI description: %w", err)
	}
	var output dataBlob
	result, _, callErr := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&input)),
		uintptr(unsafe.Pointer(descriptionPointer)),
		uintptr(unsafe.Pointer(&entropy)),
		0,
		0,
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&output)),
	)
	if result == 0 {
		return nil, fmt.Errorf("CryptProtectData: %w", callError(callErr))
	}
	return copyAndFree(output, false)
}

func (p DPAPIProtector) Unprotect(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, errors.New("cannot unprotect an empty secret")
	}
	input := blob(ciphertext)
	entropy := blob(p.entropy)
	var output dataBlob
	result, _, callErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&input)),
		0,
		uintptr(unsafe.Pointer(&entropy)),
		0,
		0,
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&output)),
	)
	if result == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", callError(callErr))
	}
	return copyAndFree(output, true)
}

func blob(value []byte) dataBlob {
	if len(value) == 0 {
		return dataBlob{}
	}
	return dataBlob{Size: uint32(len(value)), Data: &value[0]}
}

func copyAndFree(value dataBlob, clearBeforeFree bool) ([]byte, error) {
	return copyAndFreeWith(value, clearBeforeFree, freeLocalMemory)
}

func copyAndFreeWith(value dataBlob, clearBeforeFree bool, free func(uintptr) error) ([]byte, error) {
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
	result, _, _ := procLocalFree.Call(pointer)
	if result != 0 {
		return errors.New("free Windows local memory: LocalFree returned a non-NULL handle")
	}
	return nil
}

func callError(err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return errors.New("call Windows API")
	}
	return err
}
