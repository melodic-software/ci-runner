//go:build !windows

package secret

import "os"

type PosixAccessController struct{}

func NewAccessController() PosixAccessController { return PosixAccessController{} }

func (PosixAccessController) Harden(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.Chmod(path, 0o700)
	}
	return os.Chmod(path, 0o600)
}

func (PosixAccessController) Verify(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return &os.PathError{Op: "verify private permissions", Path: path, Err: os.ErrPermission}
	}
	return nil
}
