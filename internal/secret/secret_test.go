package secret

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
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

type testPrivateKeySource struct {
	*bytes.Reader
	commit func() error
	close  func() error
}

func (s *testPrivateKeySource) CommitRemoval() error {
	if s.commit == nil {
		return nil
	}
	return s.commit()
}

func (s *testPrivateKeySource) Close() error {
	if s.close == nil {
		return nil
	}
	return s.close()
}

func testSourceOpener(t *testing.T, expectedPath string, commit func(string) error) func(string) (privateKeySource, error) {
	t.Helper()
	return func(path string) (privateKeySource, error) {
		if path != expectedPath {
			return nil, fmt.Errorf("source path = %q, want %q", path, expectedPath)
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		commitFile := commit
		if commitFile == nil {
			commitFile = os.Remove
		}
		return &testPrivateKeySource{
			Reader: bytes.NewReader(value),
			commit: func() error { return commitFile(path) },
		}, nil
	}
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
				openSource: testSourceOpener(t, source, func(path string) error {
					events = append(events, "remove:"+path)
					return os.Remove(path)
				}),
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
			var persisted envelope
			if err := json.Unmarshal(stored, &persisted); err != nil {
				t.Fatalf("decode persisted envelope: %v", err)
			}
			if persisted.SchemaVersion != secretSchemaVersion || persisted.Fingerprint != result.Fingerprint {
				t.Fatalf("new import envelope = schema %d fingerprint %q, want v%d canonical fingerprint %q", persisted.SchemaVersion, persisted.Fingerprint, secretSchemaVersion, result.Fingerprint)
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

func TestImporterSourceRemovalFailurePreservesProtectedDestinationForManualCleanup(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "input.pem")
	wantSource := writeTestPrivateKey(t, source)
	destination := filepath.Join(directory, "secrets", "organization-host.dpapi")
	removeErr := errors.New("source is still open")

	_, err := (Importer{
		Protector:  reversibleProtector{},
		BitLocker:  fakeBitLocker{},
		ACL:        &fakeACL{},
		openSource: testSourceOpener(t, source, func(string) error { return removeErr }),
	}).Import(context.Background(), source, destination)
	if !errors.Is(err, removeErr) || !strings.Contains(err.Error(), "protected destination") || !strings.Contains(err.Error(), "retained") || !strings.Contains(err.Error(), "manual cleanup required") {
		t.Fatalf("expected source-removal failure with retained destination, got %v", err)
	}
	gotSource, readErr := os.ReadFile(source)
	if readErr != nil || !bytes.Equal(gotSource, wantSource) {
		t.Fatalf("failed import did not preserve source: read=%v equal=%t", readErr, bytes.Equal(gotSource, wantSource))
	}
	if _, _, loadErr := (Store{Protector: reversibleProtector{}}).LoadPrivateKey(destination); loadErr != nil {
		t.Fatalf("verified protected destination was not retained: %v", loadErr)
	}
}

func TestImporterNeverDeletesReplacementWhenSourceIdentityBecomesAmbiguous(t *testing.T) {
	for _, test := range []struct {
		name        string
		replacement bool
	}{
		{name: "pathname replaced", replacement: true},
		{name: "pathname missing", replacement: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			source := filepath.Join(directory, "input.pem")
			original := writeTestPrivateKey(t, source)
			moved := filepath.Join(directory, "original-moved.pem")
			destination := filepath.Join(directory, "secrets", "organization-host.dpapi")
			replacement := []byte("replacement must never be deleted")
			identityErr := errors.New("source pathname identity changed")

			_, err := (Importer{
				Protector: reversibleProtector{},
				BitLocker: fakeBitLocker{},
				ACL:       &fakeACL{},
				openSource: testSourceOpener(t, source, func(string) error {
					if err := os.Rename(source, moved); err != nil {
						return err
					}
					if test.replacement {
						if err := os.WriteFile(source, replacement, 0o600); err != nil {
							return err
						}
					}
					return identityErr
				}),
			}).Import(context.Background(), source, destination)
			if !errors.Is(err, identityErr) || !strings.Contains(err.Error(), "manual cleanup required") || !strings.Contains(err.Error(), "retained") {
				t.Fatalf("expected conservative identity failure, got %v", err)
			}
			gotOriginal, readErr := os.ReadFile(moved)
			if readErr != nil || !bytes.Equal(gotOriginal, original) {
				t.Fatalf("original source was not preserved: read=%v equal=%t", readErr, bytes.Equal(gotOriginal, original))
			}
			if test.replacement {
				gotReplacement, readErr := os.ReadFile(source)
				if readErr != nil || !bytes.Equal(gotReplacement, replacement) {
					t.Fatalf("replacement was changed or deleted: read=%v equal=%t", readErr, bytes.Equal(gotReplacement, replacement))
				}
			} else if _, statErr := os.Lstat(source); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("source pathname unexpectedly recreated: %v", statErr)
			}
			if _, _, loadErr := (Store{Protector: reversibleProtector{}}).LoadPrivateKey(destination); loadErr != nil {
				t.Fatalf("protected destination was not retained: %v", loadErr)
			}
		})
	}
}

func TestImporterReportsProtectedDestinationRollbackFailure(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "input.pem")
	writeTestPrivateKey(t, source)
	destination := filepath.Join(directory, "secrets", "organization-host.dpapi")
	secureErr := errors.New("destination ACL denied")
	rollbackErr := errors.New("destination ACL denies deletion")
	acl := &fakeACL{harden: func(path string) error {
		if path == destination {
			return secureErr
		}
		return nil
	}}

	_, err := (Importer{
		Protector:  reversibleProtector{},
		BitLocker:  fakeBitLocker{},
		ACL:        acl,
		openSource: testSourceOpener(t, source, nil),
		RemoveFile: func(path string) error {
			if path == destination {
				return rollbackErr
			}
			return fmt.Errorf("unexpected removal path %q", path)
		},
	}).Import(context.Background(), source, destination)
	if !errors.Is(err, secureErr) || !strings.Contains(err.Error(), rollbackErr.Error()) || !strings.Contains(err.Error(), "manual cleanup required") {
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
		Protector:  failingUnprotectProtector{err: verifyErr},
		BitLocker:  fakeBitLocker{},
		ACL:        &fakeACL{},
		openSource: testSourceOpener(t, source, nil),
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
		Protector:  reversibleProtector{},
		BitLocker:  fakeBitLocker{},
		ACL:        &fakeACL{},
		openSource: testSourceOpener(t, source, nil),
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

func TestStoreLoadsLegacyV1HexFingerprintAndReturnsCanonicalBase64(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "legacy.dpapi")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	legacyFingerprint, err := legacyPublicKeyFingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	canonicalFingerprint, err := publicKeyFingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := (reversibleProtector{}).Protect(der, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(envelope{
		SchemaVersion: legacySecretSchemaVersion,
		Algorithm:     secretAlgorithm,
		Encoding:      secretEncoding,
		Fingerprint:   legacyFingerprint,
		ImportedAt:    time.Date(2026, 7, 1, 1, 2, 3, 0, time.UTC),
		Ciphertext:    ciphertext,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, metadata, err := (Store{Protector: reversibleProtector{}}).LoadPrivateKey(path)
	if err != nil {
		t.Fatalf("LoadPrivateKey legacy v1: %v", err)
	}
	defer clearRSAPrivateKey(loaded)
	if loaded.N.Cmp(key.N) != 0 {
		t.Fatal("legacy v1 key changed during load")
	}
	if metadata.Fingerprint != canonicalFingerprint {
		t.Fatalf("reported fingerprint = %q, want canonical Base64 %q", metadata.Fingerprint, canonicalFingerprint)
	}
}

func TestStoreFingerprintEncodingIsBoundToEnvelopeSchema(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	legacyFingerprint, _ := legacyPublicKeyFingerprint(&key.PublicKey)
	canonicalFingerprint, _ := publicKeyFingerprint(&key.PublicKey)
	ciphertext, _ := (reversibleProtector{}).Protect(der, "test")
	for _, test := range []struct {
		name        string
		schema      int
		fingerprint string
	}{
		{name: "v1 rejects Base64", schema: legacySecretSchemaVersion, fingerprint: canonicalFingerprint},
		{name: "v2 rejects hex", schema: secretSchemaVersion, fingerprint: legacyFingerprint},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret.dpapi")
			value, err := json.Marshal(envelope{
				SchemaVersion: test.schema,
				Algorithm:     secretAlgorithm,
				Encoding:      secretEncoding,
				Fingerprint:   test.fingerprint,
				ImportedAt:    time.Now().UTC(),
				Ciphertext:    ciphertext,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, value, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := (Store{Protector: reversibleProtector{}}).LoadPrivateKey(path); err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
				t.Fatalf("expected schema-bound fingerprint rejection, got %v", err)
			}
		})
	}
}

type failingExclusiveFile struct {
	writeErr error
	syncErr  error
	closeErr error
}

func (f *failingExclusiveFile) Write(value []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(value), nil
}

func (f *failingExclusiveFile) Sync() error  { return f.syncErr }
func (f *failingExclusiveFile) Close() error { return f.closeErr }

func TestWriteExclusiveSurfacesCleanupFailuresForManualCleanup(t *testing.T) {
	writeErr := errors.New("write failed")
	syncErr := errors.New("sync failed")
	closeErr := errors.New("close failed")
	cleanupErr := errors.New("remove failed")
	for _, test := range []struct {
		name string
		file *failingExclusiveFile
		want error
	}{
		{name: "write", file: &failingExclusiveFile{writeErr: writeErr}, want: writeErr},
		{name: "sync", file: &failingExclusiveFile{syncErr: syncErr}, want: syncErr},
		{name: "close", file: &failingExclusiveFile{closeErr: closeErr}, want: closeErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			removed := 0
			err := writeExclusiveWith("protected.dpapi", []byte("ciphertext"),
				func(path string, flags int, mode os.FileMode) (exclusiveFile, error) {
					if path != "protected.dpapi" || flags&os.O_EXCL == 0 || mode != 0o600 {
						t.Fatalf("unexpected exclusive-open contract: path=%q flags=%d mode=%#o", path, flags, mode)
					}
					return test.file, nil
				},
				func(string) error {
					removed++
					return cleanupErr
				},
			)
			if !errors.Is(err, test.want) || !strings.Contains(err.Error(), cleanupErr.Error()) || !strings.Contains(err.Error(), "manual cleanup required") {
				t.Fatalf("expected primary and cleanup diagnostics, got %v", err)
			}
			if removed != 1 {
				t.Fatalf("cleanup removal attempts = %d, want 1", removed)
			}
		})
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
