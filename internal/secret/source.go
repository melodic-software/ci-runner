package secret

import (
	"errors"
	"io"
	"os"
)

// privateKeySource stays open from validation through deletion. CommitRemoval
// deletes only that opened object, never the original pathname.
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
