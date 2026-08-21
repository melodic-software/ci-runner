//go:build windows

package host

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ShutdownReboot restarts the machine through the trusted System32
// shutdown.exe, matching the schtasks.exe child-process host-adapter pattern.
type ShutdownReboot struct {
	Runner         CommandRunner
	executablePath string
}

func (s ShutdownReboot) Reboot(ctx context.Context, delay time.Duration, comment string) error {
	if delay < 0 {
		return errors.New("reboot delay must be non-negative")
	}
	executable, err := resolveExecutable(s.executablePath, func() (string, error) {
		return trustedSystemExecutable("shutdown.exe")
	})
	if err != nil {
		return err
	}
	args := rebootArgs(delay, comment)
	if _, err := resolvedCommandRunner(s.Runner).Run(ctx, executable, args...); err != nil {
		return fmt.Errorf("restart machine: %w", err)
	}
	return nil
}
