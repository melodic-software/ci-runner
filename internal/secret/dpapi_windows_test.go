//go:build windows

package secret

import (
	"bytes"
	"testing"
)

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
