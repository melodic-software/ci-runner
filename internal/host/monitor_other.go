//go:build !windows

package host

import (
	"context"
	"time"

	"github.com/melodic-software/ci-runner/internal/model"
)

type WindowsPowerMonitor struct{}

func (WindowsPowerMonitor) Snapshot(context.Context) (model.PowerSnapshot, error) {
	return model.PowerSnapshot{}, errWindowsHostRequired
}

type WindowsPresenceMonitor struct{}

func (WindowsPresenceMonitor) InputIdle() (time.Duration, error) {
	return 0, errWindowsHostRequired
}

type WindowsResourceMonitor struct{}

func (*WindowsResourceMonitor) Snapshot(context.Context) (model.ResourceSnapshot, error) {
	return model.ResourceSnapshot{}, errWindowsHostRequired
}
