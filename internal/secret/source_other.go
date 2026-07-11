//go:build !windows

package secret

import (
	"fmt"
	"os"
)

// Private-key import is a Windows-only operation because its production
// prerequisites are current-user DPAPI and BitLocker. Refuse to emulate the
// identity-bound delete contract with a pathname-only unlink on other hosts.
func openPrivateKeySource(path string) (privateKeySource, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect private-key source: %w", err)
	}
	if err := inspectPrivateKeySource(info); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("identity-bound private-key source deletion requires Windows")
}
