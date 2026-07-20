//go:build windows

package secret

import (
	"bytes"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCopyAndFreeReleasesZeroLengthAllocation(t *testing.T) {
	allocation := byte(0)
	pointer := uintptr(unsafe.Pointer(&allocation))
	freed := uintptr(0)
	result, err := copyAndFreeWith(windows.DataBlob{Data: &allocation}, true, func(value uintptr) error {
		freed = value
		return nil
	})
	if err != nil {
		t.Fatalf("copyAndFreeWith: %v", err)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
	if freed != pointer {
		t.Fatalf("freed pointer = %#x, want %#x", freed, pointer)
	}
}

func TestDPAPIProtectorRoundTripUsesCurrentIdentity(t *testing.T) {
	protector := NewDPAPIProtector()
	plaintext := []byte("native-dpapi-round-trip")
	protected, err := protector.Protect(plaintext, "ci-runner native test")
	if err != nil {
		t.Fatalf("Protect: %v", err)
	}
	if bytes.Equal(protected, plaintext) {
		t.Fatal("DPAPI output equals plaintext")
	}
	got, err := protector.Unprotect(protected)
	if err != nil {
		t.Fatalf("Unprotect: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

func TestWindowsAccessControllerAppliesAndVerifiesExactDACL(t *testing.T) {
	controller := NewAccessController()
	directory := t.TempDir()
	if err := controller.Harden(directory); err != nil {
		t.Fatalf("Harden directory: %v", err)
	}
	if err := controller.Verify(directory); err != nil {
		t.Fatalf("Verify directory: %v", err)
	}
}
