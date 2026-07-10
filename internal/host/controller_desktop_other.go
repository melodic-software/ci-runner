//go:build !windows

package host

import "context"

func NewControllerDesktopAdapter() ControllerDesktopAdapter {
	return ControllerDesktopAdapter{
		Desktop: unsupportedDesktop{},
		Docker:  unsupportedDocker{},
		WSL:     unsupportedWSL{},
	}
}

type unsupportedDesktop struct{}

func (unsupportedDesktop) Status(context.Context) (DesktopStatus, error) {
	return DesktopStatusUnknown, errWindowsHostRequired
}
func (unsupportedDesktop) Start(context.Context) error { return errWindowsHostRequired }
func (unsupportedDesktop) Stop(context.Context) error  { return errWindowsHostRequired }

type unsupportedDocker struct{}

func (unsupportedDocker) EngineReachable(context.Context) (bool, error) {
	return false, errWindowsHostRequired
}
func (unsupportedDocker) Containers(context.Context) ([]Container, error) {
	return nil, errWindowsHostRequired
}

type unsupportedWSL struct{}

func (unsupportedWSL) Running(context.Context) ([]string, error) {
	return nil, errWindowsHostRequired
}
func (unsupportedWSL) Shutdown(context.Context) error { return errWindowsHostRequired }
