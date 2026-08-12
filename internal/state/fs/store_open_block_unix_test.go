//go:build !windows

package statefs

import "os"

func blockObservedOpen(path string) (func(), error) {
	if err := os.Chmod(path, 0o000); err != nil {
		return nil, err
	}
	return func() { _ = os.Chmod(path, 0o600) }, nil
}
