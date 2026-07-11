// Package secret imports and loads GitHub App private keys without exposing
// their plaintext to worker containers, process arguments, or persistent
// controller state.
package secret

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/melodic-software/ci-runner/internal/scaleset"
)

const (
	secretSchemaVersion = 1
	secretAlgorithm     = "DPAPI-CurrentUser"
	secretEncoding      = "PKCS8-DER"
)

var secretIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type Protector interface {
	Protect(plaintext []byte, description string) ([]byte, error)
	Unprotect(ciphertext []byte) ([]byte, error)
}

type BitLockerVerifier interface {
	VerifyProtected(context.Context, string) error
}

type AccessController interface {
	Harden(string) error
}

type ImportResult struct {
	Path        string    `json:"path"`
	Fingerprint string    `json:"fingerprint"`
	ImportedAt  time.Time `json:"importedAt"`
}

type envelope struct {
	SchemaVersion int       `json:"schemaVersion"`
	Algorithm     string    `json:"algorithm"`
	Encoding      string    `json:"encoding"`
	Fingerprint   string    `json:"fingerprint"`
	ImportedAt    time.Time `json:"importedAt"`
	Ciphertext    []byte    `json:"ciphertext"`
}

type Importer struct {
	Protector  Protector
	BitLocker  BitLockerVerifier
	ACL        AccessController
	Now        func() time.Time
	RemoveFile func(string) error
}

func (i Importer) Import(ctx context.Context, sourcePath, destinationPath string) (ImportResult, error) {
	if i.Protector == nil || i.BitLocker == nil || i.ACL == nil {
		return ImportResult{}, errors.New("secret importer dependencies are incomplete")
	}
	if strings.TrimSpace(sourcePath) == "" || strings.TrimSpace(destinationPath) == "" {
		return ImportResult{}, errors.New("source path and destination path are required")
	}

	destinationDirectory := filepath.Dir(destinationPath)
	if err := i.BitLocker.VerifyProtected(ctx, destinationDirectory); err != nil {
		return ImportResult{}, fmt.Errorf("BitLocker precondition: %w", err)
	}
	sourceInfo, err := os.Lstat(sourcePath)
	if err != nil {
		return ImportResult{}, fmt.Errorf("inspect private-key source: %w", err)
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 {
		return ImportResult{}, errors.New("private-key source must not be a symbolic link")
	}
	if !sourceInfo.Mode().IsRegular() {
		return ImportResult{}, errors.New("private-key source must be a regular file")
	}
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return ImportResult{}, fmt.Errorf("read private key: %w", err)
	}
	defer zero(raw)

	key, err := parseRSAPrivateKey(raw)
	if err != nil {
		return ImportResult{}, err
	}
	defer clearRSAPrivateKey(key)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return ImportResult{}, fmt.Errorf("normalize private key: %w", err)
	}
	defer zero(der)

	fingerprint, err := publicKeyFingerprint(&key.PublicKey)
	if err != nil {
		return ImportResult{}, err
	}
	protected, err := i.Protector.Protect(der, "ci-runner GitHub App key "+fingerprint)
	if err != nil {
		return ImportResult{}, fmt.Errorf("protect private key with current-user DPAPI: %w", err)
	}
	defer zero(protected)

	now := time.Now().UTC()
	if i.Now != nil {
		now = i.Now().UTC()
	}
	encoded, err := json.MarshalIndent(envelope{
		SchemaVersion: secretSchemaVersion,
		Algorithm:     secretAlgorithm,
		Encoding:      secretEncoding,
		Fingerprint:   fingerprint,
		ImportedAt:    now,
		Ciphertext:    protected,
	}, "", "  ")
	if err != nil {
		return ImportResult{}, fmt.Errorf("encode protected secret: %w", err)
	}
	encoded = append(encoded, '\n')
	defer zero(encoded)

	if err := os.MkdirAll(destinationDirectory, 0o700); err != nil {
		return ImportResult{}, fmt.Errorf("create secret directory: %w", err)
	}
	if err := i.ACL.Harden(destinationDirectory); err != nil {
		return ImportResult{}, fmt.Errorf("secure secret directory: %w", err)
	}
	if err := writeExclusive(destinationPath, encoded); err != nil {
		return ImportResult{}, err
	}
	if err := i.ACL.Harden(destinationPath); err != nil {
		return ImportResult{}, i.rollbackProtectedDestination(destinationPath, fmt.Errorf("secure protected secret: %w", err))
	}

	// Prove the persisted envelope can be decrypted and parsed under the
	// current identity before deleting the only plaintext source supplied by
	// the operator. ACL hardening deliberately precedes this readback so a bad
	// ACL transaction cannot strand an unusable protected credential.
	loaded, metadata, err := (Store{Protector: i.Protector}).LoadPrivateKey(destinationPath)
	if err != nil {
		return ImportResult{}, i.rollbackProtectedDestination(destinationPath, fmt.Errorf("verify protected secret before removing plaintext source: %w", err))
	}
	clearRSAPrivateKey(loaded)
	if metadata.Fingerprint != fingerprint || !metadata.ImportedAt.Equal(now) {
		return ImportResult{}, i.rollbackProtectedDestination(destinationPath, errors.New("verify protected secret before removing plaintext source: metadata mismatch"))
	}
	if err := ctx.Err(); err != nil {
		return ImportResult{}, i.rollbackProtectedDestination(destinationPath, fmt.Errorf("import canceled before removing plaintext source: %w", err))
	}

	if err := i.removeFile(sourcePath); err != nil {
		return ImportResult{}, i.rollbackProtectedDestination(destinationPath, fmt.Errorf("remove plaintext source %q: %w", sourcePath, err))
	}

	return ImportResult{Path: destinationPath, Fingerprint: fingerprint, ImportedAt: now}, nil
}

func (i Importer) removeFile(path string) error {
	if i.RemoveFile != nil {
		return i.RemoveFile(path)
	}
	return os.Remove(path)
}

func (i Importer) rollbackProtectedDestination(path string, cause error) error {
	if err := i.removeFile(path); err != nil {
		return fmt.Errorf("%w; also failed to roll back protected destination %q: %v; manual cleanup required", cause, path, err)
	}
	return fmt.Errorf("%w; protected destination rolled back", cause)
}

type Store struct {
	Protector Protector
	Directory string
}

// Inspect proves that a configured current-user secret exists, decrypts, and
// still matches its recorded public-key fingerprint. It returns only safe
// metadata and clears the parsed private key before returning.
func (s Store) Inspect(ctx context.Context, secretID string) (ImportResult, error) {
	if err := ctx.Err(); err != nil {
		return ImportResult{}, err
	}
	path, err := s.path(secretID)
	if err != nil {
		return ImportResult{}, err
	}
	key, result, err := s.LoadPrivateKey(path)
	if err != nil {
		return ImportResult{}, err
	}
	clearRSAPrivateKey(key)
	if err := ctx.Err(); err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

func (s Store) PrivateKey(ctx context.Context, secretID string) (scaleset.SecretMaterial, error) {
	if err := ctx.Err(); err != nil {
		return scaleset.SecretMaterial{}, err
	}
	path, err := s.path(secretID)
	if err != nil {
		return scaleset.SecretMaterial{}, err
	}
	key, _, err := s.LoadPrivateKey(path)
	if err != nil {
		return scaleset.SecretMaterial{}, err
	}
	defer clearRSAPrivateKey(key)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return scaleset.SecretMaterial{}, fmt.Errorf("marshal protected RSA key: %w", err)
	}
	defer zero(der)
	canonicalPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if len(canonicalPEM) == 0 {
		return scaleset.SecretMaterial{}, errors.New("encode protected RSA key as PEM")
	}
	defer zero(canonicalPEM)
	if err := ctx.Err(); err != nil {
		return scaleset.SecretMaterial{}, err
	}
	return scaleset.NewSecretMaterial(canonicalPEM), nil
}

func (s Store) path(secretID string) (string, error) {
	if !secretIDPattern.MatchString(secretID) {
		return "", fmt.Errorf("invalid secret ID %q", secretID)
	}
	if !filepath.IsAbs(s.Directory) {
		return "", errors.New("secret directory must be absolute")
	}
	return filepath.Join(s.Directory, secretID+".dpapi"), nil
}

func (s Store) LoadPrivateKey(path string) (*rsa.PrivateKey, ImportResult, error) {
	if s.Protector == nil {
		return nil, ImportResult{}, errors.New("secret protector is required")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, ImportResult{}, fmt.Errorf("read protected secret: %w", err)
	}
	defer zero(encoded)
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var value envelope
	if err := decoder.Decode(&value); err != nil {
		return nil, ImportResult{}, fmt.Errorf("decode protected secret: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, ImportResult{}, err
	}
	if value.SchemaVersion != secretSchemaVersion || value.Algorithm != secretAlgorithm || value.Encoding != secretEncoding {
		return nil, ImportResult{}, errors.New("unsupported protected-secret format")
	}
	if value.ImportedAt.IsZero() {
		return nil, ImportResult{}, errors.New("protected-secret import timestamp is missing")
	}
	plaintext, err := s.Protector.Unprotect(value.Ciphertext)
	if err != nil {
		return nil, ImportResult{}, fmt.Errorf("unprotect private key with current-user DPAPI: %w", err)
	}
	defer zero(plaintext)
	parsed, err := x509.ParsePKCS8PrivateKey(plaintext)
	if err != nil {
		return nil, ImportResult{}, fmt.Errorf("parse protected PKCS#8 key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, ImportResult{}, errors.New("protected key is not RSA")
	}
	if err := key.Validate(); err != nil {
		return nil, ImportResult{}, fmt.Errorf("validate protected RSA key: %w", err)
	}
	fingerprint, err := publicKeyFingerprint(&key.PublicKey)
	if err != nil {
		return nil, ImportResult{}, err
	}
	if fingerprint != value.Fingerprint {
		return nil, ImportResult{}, errors.New("protected-secret fingerprint mismatch")
	}
	return key, ImportResult{Path: path, Fingerprint: fingerprint, ImportedAt: value.ImportedAt}, nil
}

func parseRSAPrivateKey(value []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(value)
	if block == nil {
		return nil, errors.New("private key must be PEM encoded")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("private key file contains trailing data")
	}
	if x509.IsEncryptedPEMBlock(block) || strings.Contains(block.Type, "ENCRYPTED") {
		return nil, errors.New("encrypted PEM private keys are not supported")
	}

	var key *rsa.PrivateKey
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var parsed any
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			var ok bool
			key, ok = parsed.(*rsa.PrivateKey)
			if !ok {
				return nil, errors.New("PKCS#8 private key is not RSA")
			}
		}
	default:
		return nil, fmt.Errorf("unsupported PEM block %q; expected RSA PRIVATE KEY or PRIVATE KEY", block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("parse RSA private key: %w", err)
	}
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("validate RSA private key: %w", err)
	}
	if key.N.BitLen() < 2048 {
		return nil, fmt.Errorf("RSA private key is %d bits; at least 2048 bits are required", key.N.BitLen())
	}
	return key, nil
}

// publicKeyFingerprint matches GitHub's documented App-key fingerprint:
// SHA-256 over the X.509 SubjectPublicKeyInfo DER, encoded with standard Base64.
func publicKeyFingerprint(key *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", fmt.Errorf("marshal RSA public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("protected-secret file contains multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode protected secret trailer: %w", err)
	}
	return nil
}

func writeExclusive(path string, value []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("protected secret %q already exists; imports never overwrite an existing key", path)
		}
		return fmt.Errorf("create protected secret: %w", err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(value); err != nil {
		return fmt.Errorf("write protected secret: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("flush protected secret: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close protected secret: %w", err)
	}
	ok = true
	return nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func clearRSAPrivateKey(key *rsa.PrivateKey) {
	if key == nil {
		return
	}
	clearBigInt(key.D)
	for _, prime := range key.Primes {
		clearBigInt(prime)
	}
	clearBigInt(key.Precomputed.Dp)
	clearBigInt(key.Precomputed.Dq)
	clearBigInt(key.Precomputed.Qinv)
	for index := range key.Precomputed.CRTValues {
		clearBigInt(key.Precomputed.CRTValues[index].Exp)
		clearBigInt(key.Precomputed.CRTValues[index].Coeff)
		clearBigInt(key.Precomputed.CRTValues[index].R)
	}
}

func clearBigInt(value *big.Int) {
	if value == nil {
		return
	}
	bits := value.Bits()
	for index := range bits {
		bits[index] = 0
	}
}

var _ scaleset.SecretStore = Store{}
