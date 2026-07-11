package secret

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
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

type failingUnprotectProtector struct{ err error }

func (failingUnprotectProtector) Protect(value []byte, description string) ([]byte, error) {
	return reversibleProtector{}.Protect(value, description)
}

func (f failingUnprotectProtector) Unprotect([]byte) ([]byte, error) {
	return nil, f.err
}

type fakeBitLocker struct{ err error }

func (f fakeBitLocker) VerifyProtected(context.Context, string) error { return f.err }

type fakeACL struct {
	paths  []string
	harden func(string) error
}

func (f *fakeACL) Harden(path string) error {
	f.paths = append(f.paths, path)
	if f.harden != nil {
		return f.harden(path)
	}
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
			events := make([]string, 0, 3)
			acl := &fakeACL{harden: func(path string) error {
				events = append(events, "harden:"+path)
				return nil
			}}
			importer := Importer{
				Protector: reversibleProtector{},
				BitLocker: fakeBitLocker{},
				ACL:       acl,
				Now:       func() time.Time { return time.Date(2026, 7, 9, 1, 2, 3, 0, time.UTC) },
				RemoveFile: func(path string) error {
					events = append(events, "remove:"+path)
					return os.Remove(path)
				},
			}
			result, err := importer.Import(context.Background(), source, destination)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if _, err := os.Lstat(source); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("plaintext source still exists after successful import: %v", err)
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
			wantEvents := []string{
				"harden:" + destinationDirectory,
				"harden:" + destination,
				"remove:" + source,
			}
			if strings.Join(events, "\n") != strings.Join(wantEvents, "\n") {
				t.Fatalf("import transaction order = %#v, want %#v", events, wantEvents)
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

func TestImporterSourceRemovalFailureRollsBackProtectedDestination(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "input.pem")
	wantSource := writeTestPrivateKey(t, source)
	destination := filepath.Join(directory, "secrets", "organization-host.dpapi")
	removeErr := errors.New("source is still open")

	_, err := (Importer{
		Protector: reversibleProtector{},
		BitLocker: fakeBitLocker{},
		ACL:       &fakeACL{},
		RemoveFile: func(path string) error {
			if path == source {
				return removeErr
			}
			return os.Remove(path)
		},
	}).Import(context.Background(), source, destination)
	if !errors.Is(err, removeErr) || !strings.Contains(err.Error(), "protected destination rolled back") {
		t.Fatalf("expected source-removal failure with rollback, got %v", err)
	}
	gotSource, readErr := os.ReadFile(source)
	if readErr != nil || !bytes.Equal(gotSource, wantSource) {
		t.Fatalf("failed import did not preserve source: read=%v equal=%t", readErr, bytes.Equal(gotSource, wantSource))
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("protected destination survived rollback: %v", statErr)
	}
}

func TestImporterReportsProtectedDestinationRollbackFailure(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "input.pem")
	writeTestPrivateKey(t, source)
	destination := filepath.Join(directory, "secrets", "organization-host.dpapi")
	removeErr := errors.New("source is still open")
	rollbackErr := errors.New("destination ACL denies deletion")

	_, err := (Importer{
		Protector: reversibleProtector{},
		BitLocker: fakeBitLocker{},
		ACL:       &fakeACL{},
		RemoveFile: func(path string) error {
			switch path {
			case source:
				return removeErr
			case destination:
				return rollbackErr
			default:
				return fmt.Errorf("unexpected removal path %q", path)
			}
		},
	}).Import(context.Background(), source, destination)
	if !errors.Is(err, removeErr) || !strings.Contains(err.Error(), rollbackErr.Error()) || !strings.Contains(err.Error(), "manual cleanup required") {
		t.Fatalf("expected source and rollback failures, got %v", err)
	}
	if _, statErr := os.Lstat(source); statErr != nil {
		t.Fatalf("failed import did not preserve source: %v", statErr)
	}
	if _, statErr := os.Lstat(destination); statErr != nil {
		t.Fatalf("expected undeletable protected destination to remain: %v", statErr)
	}
}

func TestImporterVerifiesProtectedDestinationBeforeRemovingSource(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "input.pem")
	writeTestPrivateKey(t, source)
	destination := filepath.Join(directory, "secrets", "organization-host.dpapi")
	verifyErr := errors.New("DPAPI readback failed")
	removed := make([]string, 0, 1)

	_, err := (Importer{
		Protector: failingUnprotectProtector{err: verifyErr},
		BitLocker: fakeBitLocker{},
		ACL:       &fakeACL{},
		RemoveFile: func(path string) error {
			removed = append(removed, path)
			return os.Remove(path)
		},
	}).Import(context.Background(), source, destination)
	if !errors.Is(err, verifyErr) || !strings.Contains(err.Error(), "verify protected secret") {
		t.Fatalf("expected protected-secret verification failure, got %v", err)
	}
	if len(removed) != 1 || removed[0] != destination {
		t.Fatalf("removal attempts = %#v, want destination rollback only", removed)
	}
	if _, statErr := os.Lstat(source); statErr != nil {
		t.Fatalf("verification failure did not preserve source: %v", statErr)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("protected destination survived verification rollback: %v", statErr)
	}
}

func TestImporterCancellationBeforeSourceRemovalPreservesSource(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "input.pem")
	writeTestPrivateKey(t, source)
	destination := filepath.Join(directory, "secrets", "organization-host.dpapi")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (Importer{
		Protector: reversibleProtector{},
		BitLocker: fakeBitLocker{},
		ACL:       &fakeACL{},
	}).Import(ctx, source, destination)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "before removing plaintext source") {
		t.Fatalf("expected pre-removal cancellation, got %v", err)
	}
	if _, statErr := os.Lstat(source); statErr != nil {
		t.Fatalf("canceled import did not preserve source: %v", statErr)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("protected destination survived cancellation rollback: %v", statErr)
	}
}

func TestImporterRejectsSymbolicLinkSourceWithoutRemovingTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.pem")
	writeTestPrivateKey(t, target)
	source := filepath.Join(directory, "input.pem")
	if err := os.Symlink(target, source); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	destination := filepath.Join(directory, "secrets", "organization-host.dpapi")

	_, err := (Importer{
		Protector: reversibleProtector{},
		BitLocker: fakeBitLocker{},
		ACL:       &fakeACL{},
	}).Import(context.Background(), source, destination)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("expected symbolic-link rejection, got %v", err)
	}
	if _, statErr := os.Lstat(source); statErr != nil {
		t.Fatalf("failed import removed source link: %v", statErr)
	}
	if _, statErr := os.Lstat(target); statErr != nil {
		t.Fatalf("failed import removed source target: %v", statErr)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("protected destination exists after rejected source: %v", statErr)
	}
}

func TestImporterRejectsWhenBitLockerIsNotProtected(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "input.pem")
	wantSource := []byte("not read because precondition runs first")
	if err := os.WriteFile(source, wantSource, 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("protection off")
	destination := filepath.Join(directory, "secrets", "organization-host.dpapi")
	_, err := (Importer{
		Protector: reversibleProtector{},
		BitLocker: fakeBitLocker{err: want},
		ACL:       &fakeACL{},
	}).Import(context.Background(), source, destination)
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want wrapped %v", err, want)
	}
	gotSource, readErr := os.ReadFile(source)
	if readErr != nil || !bytes.Equal(gotSource, wantSource) {
		t.Fatalf("failed import did not preserve source: read=%v equal=%t", readErr, bytes.Equal(gotSource, wantSource))
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("protected destination exists after failed precondition: %v", statErr)
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

func TestPublicKeyFingerprintMatchesGitHubVerificationFormat(t *testing.T) {
	const publicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAodLwG+3TcQfJ3RafRUvj
sH0fcSWXHoIf6XkYVkncRb4dA8QGlBwCyyRM3EMv1fe44lLxJ3Ae/GU/UbXa3g9g
J1TzAYafBoD05eIVIGCH0MtmdP/KTP+dTSkJWm+BMjZ8Sf+uUdl3J35mENG50TWZ
4ZHgQGmPYRzjCKktGdWSsPV19wl3UlHG+vntMHhzUQhXv+UZuD1UJHijvOLA7prw
cjIEOjI7i921potsN+KIL9Xzl2qJTltEIS05jU+JcoQlGaMJI+KmWQrk431xgNm1
OTJ2QmLimlpnrMDwTbdY7FoYGHUS6y2yshMrow6oQZ3zJmbg4Lrt6XqV2HjKmaeh
/wIDAQAB
-----END PUBLIC KEY-----`
	block, rest := pem.Decode([]byte(publicKeyPEM))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatal("decode deterministic public-key fixture")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("fixture type = %T, want *rsa.PublicKey", parsed)
	}

	got, err := publicKeyFingerprint(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	const want = "2YdLSoCbzyTyBhMUy0/F8IdGR2MEd9tDpSUIV0mrAhI="
	if got != want {
		t.Fatalf("fingerprint = %q, want GitHub-compatible Base64 %q", got, want)
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

func writeTestPrivateKey(t *testing.T, path string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPEM
}
