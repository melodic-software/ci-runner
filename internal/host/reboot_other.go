//go:build !windows

package host

import (
	"context"
	"time"
)

// ShutdownReboot is the Linux stub for the Windows shutdown.exe adapter.
type ShutdownReboot struct{}

func (ShutdownReboot) Reboot(context.Context, time.Duration, string) error {
	return errWindowsHostRequired
}
