package secret

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reversibleProtector struct{}

func (reversibleProtector) Protect(value []byte, _ string) ([]byte, error) {
	result := append([]byte(nil), value...)
	for index := range result {
		result[index] ^= 0xaa
	}
	return result, nil
}

func (reversibleProtector) Unprotect(value []byte) ([]byte, error) {
	return reversibleProtector{}.Protect(value, "")
}

type fakeBitLocker struct{ err error }

func (f fakeBitLocker) VerifyProtected(context.Context, string) error { return f.err }

type fakeACL struct{ paths []string }

func (f *fakeACL) Harden(path string) error {
	f.paths = append(f.paths, path)
	return nil
}

func TestImporterAcceptsPKCS1AndPKCS8WithoutPersistingPlaintext(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"pkcs1": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	tests["pkcs8"] = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})

	for name, keyPEM := range tests {
		name, keyPEM := name, keyPEM
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			source := filepath.Join(directory, "input.pem")
			if err := os.WriteFile(source, keyPEM, 0o600); err != nil {
				t.Fatal(err)
			}
			destinationDirectory := filepath.Join(directory, "secrets")
			destination := filepath.Join(destinationDirectory, "organization-host.dpapi")
			acl := &fakeACL{}
			importer := Importer{
				Protector: reversibleProtector{},
				BitLocker: fakeBitLocker{},
				ACL:       acl,
				Now:       func() time.Time { return time.Date(2026, 7, 9, 1, 2, 3, 0, time.UTC) },
			}
			result, err := importer.Import(context.Background(), source, destination)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			stored, err := os.ReadFile(result.Path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(stored, []byte("PRIVATE KEY")) || bytes.Contains(stored, keyPEM) {
				t.Fatal("protected file contains plaintext key material")
			}
			store := Store{Protector: reversibleProtector{}, Directory: destinationDirectory}
			loaded, metadata, err := store.LoadPrivateKey(result.Path)
			if err != nil {
				t.Fatalf("LoadPrivateKey: %v", err)
			}
			if loaded.N.Cmp(key.N) != 0 || metadata.Fingerprint != result.Fingerprint {
				t.Fatal("loaded key or fingerprint differs")
			}
			if len(acl.paths) != 2 {
				t.Fatalf("expected directory and file ACL hardening, got %#v", acl.paths)
			}
			material, err := store.PrivateKey(context.Background(), "organization-host")
			if err != nil {
				t.Fatalf("PrivateKey: %v", err)
			}
			revealed := material.Reveal()
			defer zero(revealed)
			if !bytes.Contains(revealed, []byte("BEGIN PRIVATE KEY")) || bytes.Contains(revealed, []byte("RSA PRIVATE KEY")) {
				t.Fatalf("expected canonical PKCS#8 PEM, got %q", revealed)
			}
			inspected, err := store.Inspect(context.Background(), "organization-host")
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if inspected.Fingerprint != result.Fingerprint || !inspected.ImportedAt.Equal(result.ImportedAt) {
				t.Fatalf("Inspect returned unexpected safe metadata: %#v", inspected)
			}
		})
	}
}

func TestImporterRejectsWhenBitLockerIsNotProtected(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "input.pem")
	if err := os.WriteFile(source, []byte("not read because precondition runs first"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("protection off")
	_, err := (Importer{
		Protector: reversibleProtector{},
		BitLocker: fakeBitLocker{err: want},
		ACL:       &fakeACL{},
	}).Import(context.Background(), source, filepath.Join(directory, "secrets", "organization-host.dpapi"))
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want wrapped %v", err, want)
	}
}

func TestParseRSAPrivateKeyRejectsTrailingData(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	value := append(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), []byte("unexpected")...)
	_, err = parseRSAPrivateKey(value)
	if err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("expected trailing-data error, got %v", err)
	}
}

func TestParseBitLockerStatusRequiresEncryptionAndProtection(t *testing.T) {
	if err := parseBitLockerStatus([]byte(`{"VolumeStatus":"FullyEncrypted","ProtectionStatus":"On"}`)); err != nil {
		t.Fatalf("expected protected volume: %v", err)
	}
	for _, value := range []string{
		`{"VolumeStatus":"EncryptionInProgress","ProtectionStatus":"On"}`,
		`{"VolumeStatus":"FullyEncrypted","ProtectionStatus":"Off"}`,
	} {
		if err := parseBitLockerStatus([]byte(value)); err == nil {
			t.Fatalf("expected rejection for %s", value)
		}
	}
}
