//go:build !windows

package host

import (
	"context"
)

type ScheduledTaskCLI struct{}

func (ScheduledTaskCLI) Start(context.Context, string) error { return errWindowsHostRequired }

type WindowsProcessObserver struct{}

func (WindowsProcessObserver) Open(uint32) (ProcessHandle, error) {
	return nil, errWindowsHostRequired
}
