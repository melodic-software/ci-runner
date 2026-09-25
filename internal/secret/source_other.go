//go:build !windows

package secret

import (
	"errors"
	"fmt"
	"os"
)

// Refuse to emulate the identity-bound delete contract with a pathname-only
// unlink; private-key import needs Windows DPAPI and BitLocker.
func openPrivateKeySource(path string) (privateKeySource, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect private-key source: %w", err)
	}
	if err := inspectPrivateKeySource(info); err != nil {
		return nil, err
	}
	return nil, errors.New("identity-bound private-key source deletion requires Windows")
}
