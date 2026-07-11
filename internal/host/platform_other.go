//go:build !windows

package host

import (
	"context"
	"errors"
)

var errWindowsHostRequired = errors.New("Docker Desktop and WSL host operations require Windows")

type unsupportedGamingHost struct{}

func (unsupportedGamingHost) Inventory(context.Context) GamingInventory {
	return GamingInventory{Problems: []string{errWindowsHostRequired.Error()}}
}

func (unsupportedGamingHost) StopAll(context.Context) error {
	return errWindowsHostRequired
}

func (unsupportedGamingHost) Verify(context.Context) (GamingVerification, error) {
	return GamingVerification{}, errWindowsHostRequired
}

func NewPlatformGamingHost() GamingHost { return unsupportedGamingHost{} }
