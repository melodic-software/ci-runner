//go:build windows

package secret

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsPrivateKeySourceLocksIdentityUntilHandleDeletion(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "input.pem")
	want := writeTestPrivateKey(t, path)
	moved := filepath.Join(directory, "moved.pem")

	source, err := openPrivateKeySource(path)
	if err != nil {
		t.Fatalf("openPrivateKeySource: %v", err)
	}
	defer func() { _ = source.Close() }()

	if err := os.Rename(path, moved); err == nil {
		t.Fatal("identity-locked source was renamed while its handle was open")
	}
	if err := os.Remove(path); err == nil {
		t.Fatal("identity-locked source was deleted by pathname while its handle was open")
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err == nil {
		t.Fatal("identity-locked source was overwritten while its handle was open")
	}
	got, err := io.ReadAll(source)
	if err != nil {
		t.Fatalf("read locked source: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("locked source contents changed")
	}
	if err := source.CommitRemoval(); err != nil {
		t.Fatalf("CommitRemoval: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists after handle-bound deletion: %v", err)
	}
}
