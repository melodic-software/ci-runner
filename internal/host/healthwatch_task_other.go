//go:build !windows

package host

import (
	"context"
	"time"
)

func InstallHealthWatchTask(context.Context, ScheduledTaskCLI, string, string, time.Duration) error {
	return errWindowsHostRequired
}
