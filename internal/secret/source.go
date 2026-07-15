package secret

import (
	"errors"
	"io"
	"os"
)

// privateKeySource keeps the exact source object open from validation through
// deletion. CommitRemoval must delete only that opened object; it must never
// fall back to deleting the original pathname after an identity ambiguity.
type privateKeySource interface {
	io.Reader
	CommitRemoval() error
	Close() error
}

func inspectPrivateKeySource(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private-key source must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return errors.New("private-key source must be a regular file")
	}
	if info.Size() == 0 {
		return errors.New("private-key source must not be empty")
	}
	return nil
}
