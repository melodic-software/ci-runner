//go:build !windows

package host

import "context"

type unsupportedEngineMemory struct{}

func (unsupportedEngineMemory) EngineMemoryTotal(context.Context) (uint64, error) {
	return 0, errWindowsHostRequired
}

func NewEngineMemoryProbe() unsupportedEngineMemory {
	return unsupportedEngineMemory{}
}
