package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReparsePointIsRejectedBeforeACLHardening(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "state")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	acl := &recordingRuntimeACL{}
	reparseErr := errors.New("reparse point")
	err := preparePrivateRuntimeDirectoryUsing(target, acl, func(string) error { return reparseErr })
	if !errors.Is(err, reparseErr) {
		t.Fatalf("error = %v", err)
	}
	if acl.hardened != 0 || acl.verified != 0 {
		t.Fatalf("ACL touched before path rejection: %#v", acl)
	}
}

func TestPathWalkRejectsAnyExistingReparseComponent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	nested := filepath.Join(root, "one", "two")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	err := ensureNoReparsePointsUsing(nested, func(path string, _ os.FileInfo) (bool, error) {
		return strings.EqualFold(filepath.Base(path), "one"), nil
	})
	if err == nil || !strings.Contains(err.Error(), "reparse point") {
		t.Fatalf("error = %v", err)
	}
}

type recordingRuntimeACL struct {
	hardened int
	verified int
}

func (a *recordingRuntimeACL) Harden(string) error { a.hardened++; return nil }
func (a *recordingRuntimeACL) Verify(string) error { a.verified++; return nil }
